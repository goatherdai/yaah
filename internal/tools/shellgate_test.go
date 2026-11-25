package tools

import (
	"strings"
	"testing"
)

// TestDangerousPatterns_AllStillFireInVerbForm is the regression table for the
// deny-list itself: every pattern must still refuse its canonical dangerous
// form now that the gate classifies the verb instead of scanning every byte of
// the line. A pattern with no canonical form here is a failure, so a new entry
// in dangerousPatterns cannot land without proving it still fires.
func TestDangerousPatterns_AllStillFireInVerbForm(t *testing.T) {
	canonical := map[string]string{
		"rm -rf /":                        "rm -rf /",
		"rm -rf ~":                        "rm -rf ~",
		"rm -rf .":                        "rm -rf .",
		"shutdown":                        "shutdown -h now",
		"reboot":                          "reboot",
		"halt":                            "halt -f",
		"mkfs":                            "mkfs.ext4 /dev/sdb1",
		"mkswap":                          "mkswap /dev/sdb1",
		"dd if=":                          "dd if=/dev/zero of=/dev/sdb",
		":(){ :|:& };:":                   ":(){ :|:& };:",
		"chmod 777 /":                     "chmod 777 /",
		"chmod -r 777 /":                  "chmod -R 777 /",
		"chown -r":                        "chown -R nobody /srv",
		`remove-item -recurse -force c:\`: `Remove-Item -Recurse -Force C:\`,
		"format-volume":                   "Format-Volume -DriveLetter D",
		"stop-computer":                   "Stop-Computer",
		"restart-computer":                "Restart-Computer",
		"clear-disk":                      "Clear-Disk -Number 1",
		"initialize-disk":                 "Initialize-Disk -Number 1",
	}
	for _, pattern := range dangerousPatterns {
		cmd, ok := canonical[pattern]
		if !ok {
			t.Fatalf("no canonical dangerous form for pattern %q: add one to this table", pattern)
		}
		t.Run(pattern, func(t *testing.T) {
			denial, refused := denyShellCommand(cmd)
			if !refused {
				t.Fatalf("denyShellCommand(%q) = allowed, want refused", cmd)
			}
			if denial.Pattern != pattern {
				t.Errorf("denial pattern = %q, want %q (denial: %s)", denial.Pattern, pattern, denial)
			}
		})
	}
	if len(canonical) != len(dangerousPatterns) {
		t.Errorf("canonical table has %d entries for %d patterns", len(canonical), len(dangerousPatterns))
	}
}

// TestDangerousPatternsAreLowercase guards a class of dead entry: the command is
// lower-cased before matching, so a mixed-case pattern can never fire (the
// list carried "chown -R" that way until yaah-0lx8).
func TestDangerousPatternsAreLowercase(t *testing.T) {
	for _, pattern := range dangerousPatterns {
		if pattern != strings.ToLower(pattern) {
			t.Errorf("pattern %q is not lower case; the scan lower-cases the command, so it can never match", pattern)
		}
	}
}

// TestDenyShellCommand_ReadOnlyDataIsNotScanned is the point of the bead: the
// arguments of a read-only command are patterns, paths and prose, so their
// vocabulary is not a command and must not be refused.
func TestDenyShellCommand_ReadOnlyDataIsNotScanned(t *testing.T) {
	cases := []string{
		// Real refusals from production sessions (bead yaah-0lx8).
		`grep -rn 'func Test.*Stop|halt.*record|Haltable' internal/...`,
		`git commit -m "fix: stop the supervised dispatch on the halt record"`,
		// Arbitrary pattern vocabulary in a search.
		`rg -n "rm -rf /|mkfs|shutdown" docs/`,
		`ag halt internal/`,
		`grep -c 'dd if=' README.md`,
		`grep -rn halt internal/tools/helpers.go | head -5`,
		// Print-only sed, and the other readers.
		`sed -n '/halt/p' internal/agent/loop.go`,
		`sed -n 's|mkfs|x|p' docs/setup.md`,
		`cat .scratch/notes-halt-kill-sudo.txt`,
		`head -20 'a file named rm -rf .txt'`,
		`tail -5 shutdown.log`,
		`less reboot-history.txt`,
		`ls -la .scratch/halt`,
		`find . -name 'halt*.go'`,
		`wc -l internal/agent/supervision/batch.go`,
		`sort -u halt-threats.txt`,
		`uniq -c mkfs.log`,
		`diff halt.txt kill.txt`,
		`file /etc/halt`,
		`stat internal/tools/bash.go`,
		`which halt`,
		`echo 'rm -rf /; mkfs -t ext4; shutdown -h now'`,
		`printf '%s\n' 'reboot; halt'`,
		// git reads.
		`git log --oneline -20 --grep halt`,
		`git log --grep 'rm -rf /'`,
		`git diff HEAD -- internal/tools/bash.go`,
		`git show 842ad081 --stat`,
		`git status --porcelain`,
		`git branch --list 'feat/*'`,
		`git rev-parse --short HEAD`,
		`git ls-files 'internal/tools/*halt*'`,
		// curl in its GET/HEAD forms.
		`curl -s https://example.invalid/halt`,
		`curl -sI https://example.invalid/reboot`,
		`curl -sSL --max-time 5 https://example.invalid/halt`,
		// jq.
		`jq '.patterns[] | select(. == "mkfs")' deny.json`,
		// Message arguments are prose, not shell.
		`git commit -m 'rm -rf / is the classic example, as is mkfs'`,
		`git commit --message "deny gate: gate the verb, not substrings"`,
		`git commit -m"task: stop scanning halt in commit messages"`,
		`git commit -F .scratch/commit-msg-halt.txt`,
		`gh pr create --title "deny gate" --body "grep 'halt' runs; rm -rf / does not"`,
		`bd update yaah-0lx8 --description "grep 'halt' must run"`,
		// A read-only head with a read-only tail, and env prefixes.
		`FOO=bar grep halt internal/tools/helpers.go`,
		`ls; cat halt.txt`,
		`git log --oneline | grep halt`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if denial, refused := denyShellCommand(cmd); refused {
				t.Errorf("denyShellCommand(%q) = refused (%s), want allowed", cmd, denial)
			}
		})
	}
}

// TestDenyShellCommand_DangerousSegmentsStillRefused covers the other half:
// dangerous verbs stay refused, and a chain is refused when ANY segment is.
func TestDenyShellCommand_DangerousSegmentsStillRefused(t *testing.T) {
	cases := []string{
		// Verb position, unwrapped.
		`rm -rf /tmp/scratch`,
		`mkfs.ext4 /dev/sdb1`,
		`dd if=/dev/zero of=/dev/sda`,
		`:(){ :|:& };:`,
		`chmod -R 777 /`,
		// A benign head does not save a dangerous tail.
		`ls; rm -rf /`,
		`ls && rm -rf /tmp/x`,
		`git status || shutdown -h now`,
		`cat halt.txt | xargs rm -rf /tmp/x`,
		// Wrappers whose arguments are another command stay scanned.
		`sudo rm -rf /`,
		`env bash -c 'rm -rf /'`,
		`xargs rm -rf /tmp/x`,
		`timeout 5 rm -rf /tmp/x`,
		`nohup shutdown -h now`,
		// Interpreters are verbs: their arguments are code, not data.
		`sh -c 'halt'`,
		`bash -c "mkfs.ext4 /dev/sdb1"`,
		`eval "mkfs.ext4 /dev/sda1"`,
		`python3 -c "import os; os.system('rm -rf /')"`,
		`python3 -c "print('shutdown')"`,
		`perl -e 'system("mkfs")'`,
		`awk 'BEGIN{system("reboot")}'`,
		// sed edits and find actions that run or delete stay scanned.
		`sed -i 's/halt/x/' internal/agent/loop.go`,
		`sed --in-place 's/halt/x/' notes.md`,
		`find . -name halt -delete`,
		`find . -exec rm -rf / {} \;`,
		// curl with a body or a method override is not a read.
		`curl -X POST -d '{"note":"rm -rf /"}' https://example.invalid`,
		`curl -s -F 'f=@halt.txt' https://example.invalid`,
		// Device redirections, with and without a read-only head.
		`echo hi > /dev/sda`,
		`grep halt /etc/hosts > /dev/sda`,
		`dd of=/dev/sda`,
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if denial, refused := denyShellCommand(cmd); !refused {
				t.Errorf("denyShellCommand(%q) = allowed, want refused", cmd)
			} else if denial.String() == "" {
				t.Errorf("denyShellCommand(%q) refused without a reason", cmd)
			}
		})
	}
}

// TestDenyShellCommand_UnclassifiableFailsClosed pins the safe direction: a
// construct the tokenizer cannot account for keeps the old whole-line scan.
func TestDenyShellCommand_UnclassifiableFailsClosed(t *testing.T) {
	cases := []string{
		`diff <(cat halt.txt) <(echo x)`, // process substitution
		`echo $(mkfs.ext4 /dev/sdb)`,     // command substitution
		"echo `reboot`",                  // backticks
		`echo 'unterminated halt`,        // unterminated quote
		`{ rm -rf /; }`,                  // unquoted grouping
		`(rm -rf /)`,                     // subshell
		`echo ${IFS}mkfs`,                // parameter expansion
		`rm -rf / # a trailing comment`,  // commented danger is still danger
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			if denial, refused := denyShellCommand(cmd); !refused {
				t.Errorf("denyShellCommand(%q) = allowed, want refused", cmd)
			} else if denial.String() == "" {
				t.Errorf("denyShellCommand(%q) refused without a reason", cmd)
			}
		})
	}
}

// TestDenyShellCommand_MessageIsDataButSegmentIsNot pins the boundary of the
// message exemption: the value is prose, a following segment is still a command.
func TestDenyShellCommand_MessageIsDataButSegmentIsNot(t *testing.T) {
	allowed := []string{
		`git commit -m "halt, kill, rm -rf / — all prose"`,
		`git commit -m "x" --amend`,
		`git tag -m "halt" v1`,
	}
	for _, cmd := range allowed {
		if denial, refused := denyShellCommand(cmd); refused {
			t.Errorf("denyShellCommand(%q) = refused (%s), want allowed", cmd, denial)
		}
	}
	refused := []string{
		`git commit -m "x"; rm -rf /`,
		`git commit -m "x" && mkfs.ext4 /dev/sdb1`,
	}
	for _, cmd := range refused {
		if denial, bad := denyShellCommand(cmd); !bad {
			t.Errorf("denyShellCommand(%q) = allowed, want refused", cmd)
		} else if denial.String() == "" {
			t.Errorf("denyShellCommand(%q) refused without a reason", cmd)
		}
	}
}

// TestDenyShellCommand_ForcePushToProtectedRef pins the structural rule the
// bead names as a hard refusal: a force push may not rewrite the mainline.
func TestDenyShellCommand_ForcePushToProtectedRef(t *testing.T) {
	refused := []string{
		`git push --force origin main`,
		`git push -f origin master`,
		`git push --force-with-lease origin main:main`,
		`git push --force origin HEAD:refs/heads/main`,
		`git push --mirror origin`,
	}
	for _, cmd := range refused {
		if denial, bad := denyShellCommand(cmd); !bad {
			t.Errorf("denyShellCommand(%q) = allowed, want refused", cmd)
		} else if denial.Rule == "" {
			t.Errorf("denyShellCommand(%q) refused without naming the rule: %s", cmd, denial)
		}
	}
	allowed := []string{
		`git push --force origin feat/0lx8-deny-gate-verbs`,
		`git push -f origin fix/tmp`,
		`git push origin main`,
		`git push --force-with-lease origin feat/x:feat/x`,
	}
	for _, cmd := range allowed {
		if denial, bad := denyShellCommand(cmd); bad {
			t.Errorf("denyShellCommand(%q) = refused (%s), want allowed", cmd, denial)
		}
	}
}

// TestDenyShellCommand_RefusalNamesPatternAndVerb pins the diagnostic: a
// refusal must say which pattern fired on which verb, in which segment.
func TestDenyShellCommand_RefusalNamesPatternAndVerb(t *testing.T) {
	denial, refused := denyShellCommand(`ls && rm -rf /tmp/x`)
	if !refused {
		t.Fatal("chained danger was allowed")
	}
	got := denial.String()
	for _, want := range []string{`"rm -rf /"`, `verb "rm"`, `segment "rm -rf /tmp/x"`} {
		if !strings.Contains(got, want) {
			t.Errorf("denial %q does not mention %s", got, want)
		}
	}

	denial, refused = denyShellCommand(`echo hi > /dev/sda`)
	if !refused {
		t.Fatal("device redirect was allowed")
	}
	if !strings.Contains(denial.String(), `/dev/sda`) {
		t.Errorf("denial %q does not name the device", denial)
	}
}

// TestDenyShellCommand_CommentLineRunsNothing pins that a comment is not a
// command: it runs nothing, so its vocabulary is not scanned.
func TestDenyShellCommand_CommentLineRunsNothing(t *testing.T) {
	for _, cmd := range []string{
		`# rm -rf / is the classic example`,
		"grep halt f.txt # mkfs later",
	} {
		if denial, refused := denyShellCommand(cmd); refused {
			t.Errorf("denyShellCommand(%q) = refused (%s), want allowed", cmd, denial)
		}
	}
}

// TestDenyShellCommand_EmptyAndOddInput is a robustness sweep: no panic, and
// nothing allowed that contains an unmistakable command.
func TestDenyShellCommand_EmptyAndOddInput(t *testing.T) {
	for _, cmd := range []string{
		"", " ", "\n", ";;", "&&", "|", "; ;", "\t\t", "   \n  ",
		"grep", "grep -n", "grep -n ''", "grep --", "sed", "git", "git -C",
		"''", `""`, `"unclosed`, "'unclosed", "\\", `>`, `>>`, `>&`,
	} {
		if denial, refused := denyShellCommand(cmd); refused {
			t.Errorf("denyShellCommand(%q) = refused (%s), want allowed", cmd, denial)
		}
	}
}

// TestTokenizeShell pins the tokenizer's reading of quoting and operators, which
// is what makes "this argument is data" defensible.
func TestTokenizeShell(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string // word texts, operators elided
	}{
		{"plain", "grep -n halt f.go", []string{"grep", "-n", "halt", "f.go"}},
		{"single quotes", `grep -n 'a b; c' f.go`, []string{"grep", "-n", "a b; c", "f.go"}},
		{"double quotes", `grep -n "a b" f.go`, []string{"grep", "-n", "a b", "f.go"}},
		{"escaped space", `grep a\ b f.go`, []string{"grep", "a b", "f.go"}},
		{"empty quotes", `grep '' f.go`, []string{"grep", "", "f.go"}},
		{"env prefix", `FOO=1 grep halt f`, []string{"FOO=1", "grep", "halt", "f"}},
		{"fd dup", `ls 2>&1`, []string{"ls", "2", "1"}},
		{"comment", "grep halt f # note", []string{"grep", "halt", "f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toks, ok := tokenizeShell(tc.cmd)
			if !ok {
				t.Fatalf("tokenizeShell(%q) failed", tc.cmd)
			}
			var got []string
			for _, tok := range toks {
				if tok.isWord() {
					got = append(got, tok.text)
				}
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("tokenizeShell(%q) words = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}

	for _, cmd := range []string{
		"echo $(id)", "echo ${x}", "echo `id`", "echo <(id)", "echo >(id)",
		"echo 'open", "echo (x)", "echo {x", "x=y(",
	} {
		if _, ok := tokenizeShell(cmd); ok {
			t.Errorf("tokenizeShell(%q) succeeded, want an unclassifiable result", cmd)
		}
	}
}
