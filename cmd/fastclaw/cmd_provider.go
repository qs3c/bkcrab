package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// providerCmd handles provider/credential management subcommands.
func providerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage provider credentials",
	}
	cmd.AddCommand(providerListCmd())
	cmd.AddCommand(providerCreateCmd())
	cmd.AddCommand(providerDeleteCmd())
	return cmd
}

func providerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all credential providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cm, err := provider.NewCredentialManagerForUser("_cli", "")
			if err != nil {
				return err
			}

			// Show stored credentials
			stored := cm.List()
			if len(stored) > 0 {
				fmt.Println("Stored credentials:")
				for _, e := range stored {
					for k, v := range e.Keys {
						fmt.Printf("  %-15s %-10s %s=%s\n", e.Name, e.Source, k, v)
					}
				}
			}

			// Show discovered from env
			discovered := cm.Discover()
			if len(discovered) > 0 {
				fmt.Println("\nDiscovered from environment:")
				for _, e := range discovered {
					for k, v := range e.Keys {
						masked := v
						if len(v) > 8 {
							masked = v[:4] + "..." + v[len(v)-4:]
						}
						fmt.Printf("  %-15s %-10s %s=%s\n", e.Name, e.Source, k, masked)
					}
				}
			}

			if len(stored) == 0 && len(discovered) == 0 {
				fmt.Println("No credentials found.")
			}

			return nil
		},
	}
}

func providerCreateCmd() *cobra.Command {
	var fromEnv bool
	var key string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a provider credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cm, err := provider.NewCredentialManagerForUser("_cli", "")
			if err != nil {
				return err
			}

			if fromEnv {
				discovered := cm.Discover()
				for _, d := range discovered {
					if d.Name == name {
						for k, v := range d.Keys {
							if err := cm.Set(name, k, v); err != nil {
								return err
							}
						}
						fmt.Printf("Provider %q created from environment.\n", name)
						return nil
					}
				}
				return fmt.Errorf("no environment variable found for provider %q", name)
			}

			if key == "" {
				return fmt.Errorf("either --from-env or --key is required")
			}

			if err := cm.Set(name, "apiKey", key); err != nil {
				return err
			}
			fmt.Printf("Provider %q created.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromEnv, "from-env", false, "Create from environment variable")
	cmd.Flags().StringVar(&key, "key", "", "API key value")
	return cmd
}

func providerDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a provider credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cm, err := provider.NewCredentialManagerForUser("_cli", "")
			if err != nil {
				return err
			}
			if err := cm.Delete(name); err != nil {
				return err
			}
			fmt.Printf("Provider %q deleted.\n", name)
			return nil
		},
	}
}
