package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
)

// skillCmd handles skill management subcommands.
func skillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills",
	}
	cmd.AddCommand(skillListCmd())
	cmd.AddCommand(skillSearchCmd())
	cmd.AddCommand(skillInstallCmd())
	cmd.AddCommand(skillUpdateCmd())
	cmd.AddCommand(skillRemoveCmd())
	cmd.AddCommand(skillInfoCmd())
	return cmd
}

func skillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all discovered skills with source",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			var globalCfg config.SkillsCfg

			loader := agent.NewSkillsLoaderWithGlobal(homeDir, ".", "", config.SkillsConfig{}, globalCfg)
			loaded := loader.LoadSkills()

			if len(loaded) == 0 {
				fmt.Println("No skills discovered.")
				return nil
			}

			fmt.Printf("%-25s %-20s %s\n", "NAME", "SOURCE", "DESCRIPTION")
			fmt.Println(strings.Repeat("-", 75))
			for _, s := range loaded {
				desc := s.Description
				if len(desc) > 40 {
					desc = desc[:37] + "..."
				}
				fmt.Printf("%-25s %-20s %s\n", s.Name, s.Layer, desc)
			}
			return nil
		},
	}
}

func skillSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Search the ClawHub skill registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := skills.NewClawHubClient()
			results, err := client.Search(args[0])
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			if len(results) == 0 {
				fmt.Println("No skills found.")
				return nil
			}

			fmt.Printf("%-25s %-10s %-10s %s\n", "SLUG", "VERSION", "DOWNLOADS", "DESCRIPTION")
			fmt.Println(strings.Repeat("-", 80))
			for _, s := range results {
				desc := s.Description
				if len(desc) > 35 {
					desc = desc[:32] + "..."
				}
				fmt.Printf("%-25s %-10s %-10d %s\n", s.Slug, s.Version, s.Downloads, desc)
			}
			return nil
		},
	}
}

func skillInstallCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "install <slug>",
		Short: "Install a skill from ClawHub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			targetDir := filepath.Join(homeDir, "skills")
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return fmt.Errorf("create skills dir: %w", err)
			}

			client := skills.NewClawHubClient()
			fmt.Printf("Installing %s...\n", slug)
			if err := client.Install(slug, version, targetDir); err != nil {
				return fmt.Errorf("install failed: %w", err)
			}

			fmt.Printf("Skill %q installed to %s\n", slug, filepath.Join(targetDir, slug))
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "specific version to install")
	return cmd
}

func skillUpdateCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "update [slug]",
		Short: "Update installed skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			targetDir := filepath.Join(homeDir, "skills")
			client := skills.NewClawHubClient()

			if all {
				installed, err := skills.ListInstalled(targetDir)
				if err != nil {
					return err
				}
				if len(installed) == 0 {
					fmt.Println("No installed skills to update.")
					return nil
				}
				for _, s := range installed {
					fmt.Printf("Updating %s...\n", s.Name)
					if err := client.Update(s.Name, targetDir); err != nil {
						fmt.Printf("  Failed: %v\n", err)
					} else {
						fmt.Printf("  Updated %s\n", s.Name)
					}
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("specify a skill slug or use --all")
			}

			slug := args[0]
			fmt.Printf("Updating %s...\n", slug)
			if err := client.Update(slug, targetDir); err != nil {
				return fmt.Errorf("update failed: %w", err)
			}
			fmt.Printf("Skill %q updated.\n", slug)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "update all installed skills")
	return cmd
}

func skillRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			skillDir := filepath.Join(homeDir, "skills", name)
			if _, err := os.Stat(skillDir); os.IsNotExist(err) {
				return fmt.Errorf("skill %q not found at %s", name, skillDir)
			}

			if err := os.RemoveAll(skillDir); err != nil {
				return fmt.Errorf("remove skill: %w", err)
			}

			fmt.Printf("Skill %q removed.\n", name)
			return nil
		},
	}
}

func skillInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <slug>",
		Short: "Show skill details from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := skills.NewClawHubClient()
			info, err := client.Info(args[0])
			if err != nil {
				return err
			}

			fmt.Printf("Name:        %s\n", info.Name)
			fmt.Printf("Slug:        %s\n", info.Slug)
			fmt.Printf("Version:     %s\n", info.Version)
			fmt.Printf("Description: %s\n", info.Description)
			fmt.Printf("Downloads:   %d\n", info.Downloads)
			return nil
		},
	}
}
