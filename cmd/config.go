package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stefanpenner/gh-audit/internal/config"
	"gopkg.in/yaml.v3"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Config management commands",
	}

	cmd.AddCommand(newConfigValidateCmd())
	cmd.AddCommand(newConfigShowCmd())

	return cmd
}

func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("config invalid: %w", err)
			}
			fmt.Println("config valid")
			return nil
		},
	}
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the resolved config",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			data, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshalling config: %w", err)
			}

			fmt.Print(string(data))
			return nil
		},
	}
}
