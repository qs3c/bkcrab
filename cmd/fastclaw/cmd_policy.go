package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/policy"
)

// policyCmd handles policy management subcommands.
func policyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Manage agent policies",
	}
	cmd.AddCommand(policyListCmd())
	cmd.AddCommand(policyShowCmd())
	return cmd
}

func policyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available policy presets",
		Run: func(cmd *cobra.Command, args []string) {
			presets := []struct {
				name string
				p    *policy.Policy
			}{
				{"permissive", policy.DefaultPolicy()},
				{"standard", policy.StandardPolicy()},
				{"restricted", policy.RestrictedPolicy()},
			}
			fmt.Printf("%-15s %s\n", "NAME", "DESCRIPTION")
			for _, pr := range presets {
				fmt.Printf("%-15s %s\n", pr.p.Name, pr.p.Description)
			}
		},
	}
}

func policyShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show policy details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			p := policy.LoadPreset(name)

			data, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		},
	}
}
