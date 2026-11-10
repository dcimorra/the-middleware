package main

import (
	"strings"
	"testing"
)

func TestTheGatewaysModelsCarryEachAliasOnce(t *testing.T) {
	if got := strings.Join(doorModels("claude", []string{"opus-5.5", "sonnet", "opus", "haiku"}), ","); got != "opus-5.5,sonnet,opus,haiku,fable" {
		t.Fatalf("claude: %s", got)
	}
	if got := strings.Join(doorModels("claude", []string{"claude-sonnet-5-5"}), ","); got != "claude-sonnet-5-5,sonnet,opus,haiku,fable" {
		t.Fatalf("claude with a full name: %s", got)
	}
	if got := strings.Join(doorModels("openai", []string{"gpt-5.5"}), ","); got != "gpt-5.5" {
		t.Fatalf("openai: %s", got)
	}
}
