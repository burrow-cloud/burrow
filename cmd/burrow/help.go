// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

package main

import "github.com/spf13/cobra"

// Burrow renders help in kubectl's order rather than Cobra's default: the description first, then
// Examples, the grouped command list, Flags, and finally a single Usage line at the bottom with a
// "Use ..." pointer. The default puts Usage near the top and the command list in the middle, which
// reads as a wall; moving Usage to the bottom keeps the description and examples in view. The
// templates are set on the root and inherited by every subcommand (Cobra walks to the parent for an
// unset template), so the whole surface is consistent.

// helpTemplate prints the long description (or short, if there is no long) followed by the usage
// block. It matches Cobra's default help template; the reordering lives in usageTemplate, which the
// usage block renders.
const helpTemplate = `{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}
{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

// usageTemplate renders, in order: Examples, Available Commands (grouped for the root, flat for a
// parent command), Flags, Global Flags, then a single Usage line and the "Use ..." pointer. Each
// section leads with a blank line so it sits one line below the previous one; the description
// printed by helpTemplate supplies the separation before the first section. The Usage line is a
// single line: `<path> [command] [flags]` for a command with subcommands, or the command's own use
// line for a runnable leaf, so a parent no longer prints the two awkward `[flags]`/`[command]`
// lines.
const usageTemplate = `{{if .HasExample}}
Examples:
{{.Example}}
{{end}}{{if .HasAvailableSubCommands}}{{if eq (len .Groups) 0}}
Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{else}}{{range $group := .Groups}}
{{.Title}}{{range $.Commands}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{end}}{{if not .AllChildCommandsHaveGroup}}
Additional Commands:{{range $.Commands}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}
Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}
{{end}}{{if .HasAvailableInheritedFlags}}
Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}
{{end}}
Usage:
{{if .HasAvailableSubCommands}}  {{.CommandPath}} [command] [flags]{{else}}  {{.UseLine}}{{end}}
{{if .HasAvailableSubCommands}}
Use "{{.CommandPath}} <command> --help" for more information about a command.
{{end}}`

// applyHelpLayout installs the kubectl-style templates on the root command so every subcommand
// inherits them.
func applyHelpLayout(root *cobra.Command) {
	root.SetHelpTemplate(helpTemplate)
	root.SetUsageTemplate(usageTemplate)
}
