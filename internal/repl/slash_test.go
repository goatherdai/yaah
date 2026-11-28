package repl

import "testing"

func TestParseSlashCommand_recognizesBuiltinCommands(t *testing.T) {
	tests := []struct {
		input string
		want  SlashCommand
	}{
		{"/exit", CmdExit},
		{"/quit", CmdExit},
		{"/clear", CmdClear},
		{"/?", CmdHelp},
		{"/help", CmdHelp},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseSlashCommand(tt.input)
			if got != tt.want {
				t.Errorf("ParseSlashCommand(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseSlashCommand_returnsNoneForRegularInput(t *testing.T) {
	tests := []string{
		"hello world",
		"/unknown-command",
		"",
		"just text",
		// The retired plan/build mode aliases are gone: they are regular
		// text now, not hidden commands.
		"/plan",
		"/build",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := ParseSlashCommand(input)
			if got != CmdNone {
				t.Errorf("ParseSlashCommand(%q) = %v, want CmdNone", input, got)
			}
		})
	}
}

func TestParseSlashCommand_argTakingCommands(t *testing.T) {
	tests := []struct {
		input string
		want  SlashCommand
	}{
		{"/agents", CmdAgents},
		{"/agent", CmdAgent},
		{"/agent helper", CmdAgent},
		{"/mode", CmdMode},
		{"/mode full", CmdMode},
		{"/model", CmdModel},
		{"/model GLM-4.6", CmdModel},
		{"/model zai/GLM-4.6", CmdModel},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ParseSlashCommand(tt.input); got != tt.want {
				t.Errorf("ParseSlashCommand(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSlashModelArg(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/model", ""},
		{"/model GLM-4.6", "GLM-4.6"},
		{"/model   zai/GLM-4.6  ", "zai/GLM-4.6"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := SlashModelArg(tt.input); got != tt.want {
				t.Errorf("SlashModelArg(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
