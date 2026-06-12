package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestApikeyCmd_Structure(t *testing.T) {
	cmd := apikeyCmd()

	if cmd.Use != "apikey" {
		t.Errorf("expected Use='apikey', got %q", cmd.Use)
	}

	subcommands := map[string]bool{
		"create": false,
		"list":   false,
		"delete": false,
		"rotate": false,
	}

	for _, sub := range cmd.Commands() {
		if _, ok := subcommands[sub.Use]; ok {
			subcommands[sub.Use] = true
		}
	}

	for name, found := range subcommands {
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestApikeyCreateCmd_RequiredFlags(t *testing.T) {
	cmd := apikeyCreateCmd()

	// --name is required
	nameFlag := cmd.Flags().Lookup("name")
	if nameFlag == nil {
		t.Fatal("missing --name flag")
	}

	// --type defaults to "user"
	typeFlag := cmd.Flags().Lookup("type")
	if typeFlag == nil {
		t.Fatal("missing --type flag")
	}
	if typeFlag.DefValue != "user" {
		t.Errorf("expected --type default='user', got %q", typeFlag.DefValue)
	}

	// --owner is optional
	ownerFlag := cmd.Flags().Lookup("owner")
	if ownerFlag == nil {
		t.Fatal("missing --owner flag")
	}
}

func TestApikeyDeleteCmd_RequiredFlags(t *testing.T) {
	cmd := apikeyDeleteCmd()

	idFlag := cmd.Flags().Lookup("id")
	if idFlag == nil {
		t.Fatal("missing --id flag")
	}

	// Verify --id is marked required
	annotations := idFlag.Annotations
	if _, ok := annotations[cobra.BashCompOneRequiredFlag]; !ok {
		t.Error("--id should be marked as required")
	}
}

func TestApikeyRotateCmd_RequiredFlags(t *testing.T) {
	cmd := apikeyRotateCmd()

	idFlag := cmd.Flags().Lookup("id")
	if idFlag == nil {
		t.Fatal("missing --id flag")
	}
}
