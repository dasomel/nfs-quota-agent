/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package completion

import (
	"fmt"
	"os"
)

// BashCompletion contains the bash completion script
const BashCompletion = `# bash completion for nfs-quota-agent

_nfs_quota_agent_completions() {
    local cur prev opts commands
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    # Main commands
    commands="run status top report cleanup ui audit version help"

    # Global options
    global_opts="--help -h"

    # Command-specific options
    run_opts="--kubeconfig --nfs-base-path --nfs-server-path --provisioner-name --process-all-nfs --sync-interval --metrics-addr --audit-log --help"
    status_opts="--path --all --help"
    top_opts="--path -n --watch --help"
    report_opts="--path --format --output --help"
    cleanup_opts="--path --kubeconfig --dry-run --force --help"
    ui_opts="--path --addr --help"
    audit_opts="--file --action --pv --namespace --start --end --fails-only --format --help"

    # Determine which command is being used
    local cmd=""
    for ((i=1; i < COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            run|status|top|report|cleanup|ui|audit|version|help)
                cmd="${COMP_WORDS[i]}"
                break
                ;;
        esac
    done

    # If no command yet, suggest commands
    if [[ -z "$cmd" ]]; then
        if [[ "$cur" == -* ]]; then
            COMPREPLY=( $(compgen -W "$global_opts" -- "$cur") )
        else
            COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        fi
        return 0
    fi

    # Command-specific completions
    case "$cmd" in
        run)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$run_opts" -- "$cur") )
            fi
            case "$prev" in
                --kubeconfig)
                    COMPREPLY=( $(compgen -f -- "$cur") )
                    ;;
                --nfs-base-path|--nfs-server-path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
                --provisioner-name)
                    COMPREPLY=( $(compgen -W "nfs.csi.k8s.io cluster.local/nfs-subdir-external-provisioner" -- "$cur") )
                    ;;
                --sync-interval)
                    COMPREPLY=( $(compgen -W "10s 30s 1m 5m" -- "$cur") )
                    ;;
                --metrics-addr)
                    COMPREPLY=( $(compgen -W ":9090 :8080 :9100" -- "$cur") )
                    ;;
            esac
            ;;
        status)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$status_opts" -- "$cur") )
            fi
            case "$prev" in
                --path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
            esac
            ;;
        top)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$top_opts" -- "$cur") )
            fi
            case "$prev" in
                --path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
                -n)
                    COMPREPLY=( $(compgen -W "5 10 20 50 100" -- "$cur") )
                    ;;
            esac
            ;;
        report)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$report_opts" -- "$cur") )
            fi
            case "$prev" in
                --path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
                --format)
                    COMPREPLY=( $(compgen -W "table json yaml csv" -- "$cur") )
                    ;;
                --output|-o)
                    COMPREPLY=( $(compgen -f -- "$cur") )
                    ;;
            esac
            ;;
        cleanup)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$cleanup_opts" -- "$cur") )
            fi
            case "$prev" in
                --path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
                --kubeconfig)
                    COMPREPLY=( $(compgen -f -- "$cur") )
                    ;;
            esac
            ;;
        ui)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$ui_opts" -- "$cur") )
            fi
            case "$prev" in
                --path)
                    COMPREPLY=( $(compgen -d -- "$cur") )
                    ;;
                --addr)
                    COMPREPLY=( $(compgen -W ":8080 :3000 :9000" -- "$cur") )
                    ;;
            esac
            ;;
        audit)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "$audit_opts" -- "$cur") )
            fi
            case "$prev" in
                --file)
                    COMPREPLY=( $(compgen -f -- "$cur") )
                    ;;
                --action)
                    COMPREPLY=( $(compgen -W "CREATE UPDATE DELETE CLEANUP" -- "$cur") )
                    ;;
                --format)
                    COMPREPLY=( $(compgen -W "table json text" -- "$cur") )
                    ;;
            esac
            ;;
    esac

    return 0
}

complete -F _nfs_quota_agent_completions nfs-quota-agent
`

// ZshCompletion contains the zsh completion script
var ZshCompletion = "#compdef nfs-quota-agent\n\n_nfs_quota_agent() {\n    local -a commands\n    local -a global_opts\n\n    commands=(\n        'run:Run the quota enforcement agent'\n        'status:Show quota status and disk usage'\n        'top:Show top directories by usage'\n        'report:Generate quota report'\n        'cleanup:Remove orphaned quotas'\n        'ui:Start web UI dashboard'\n        'audit:Query audit logs'\n        'version:Print version information'\n        'help:Show help'\n    )\n\n    global_opts=(\n        '--help[Show help]'\n        '-h[Show help]'\n    )\n\n    _arguments -C \\\n        '1:command:->command' \\\n        '*::options:->options'\n\n    case $state in\n        command)\n            _describe -t commands 'nfs-quota-agent commands' commands\n            ;;\n        options)\n            case $words[1] in\n                run)\n                    _arguments \\\n                        '--kubeconfig[Path to kubeconfig file]:file:_files' \\\n                        '--nfs-base-path[Local path where NFS is mounted]:directory:_directories' \\\n                        '--nfs-server-path[NFS server'\\''s export path]:directory:_directories' \\\n                        '--provisioner-name[Provisioner name to filter PVs]:provisioner:(nfs.csi.k8s.io cluster.local/nfs-subdir-external-provisioner)' \\\n                        '--process-all-nfs[Process all NFS PVs regardless of provisioner]' \\\n                        '--sync-interval[Interval between quota syncs]:interval:(10s 30s 1m 5m)' \\\n                        '--metrics-addr[Address for Prometheus metrics endpoint]:address:(:9090 :8080 :9100)' \\\n                        '--audit-log[Path to audit log file]:file:_files' \\\n                        '--help[Show help]'\n                    ;;\n                status)\n                    _arguments \\\n                        '--path[NFS export path to check]:directory:_directories' \\\n                        '--all[Show all directories]' \\\n                        '--help[Show help]'\n                    ;;\n                top)\n                    _arguments \\\n                        '--path[NFS export path to check]:directory:_directories' \\\n                        '-n[Number of top directories to show]:count:(5 10 20 50 100)' \\\n                        '--watch[Watch mode (refresh every 5s)]' \\\n                        '--help[Show help]'\n                    ;;\n                report)\n                    _arguments \\\n                        '--path[NFS export path to check]:directory:_directories' \\\n                        '--format[Output format]:format:(table json yaml csv)' \\\n                        '--output[Output file]:file:_files' \\\n                        '--help[Show help]'\n                    ;;\n                cleanup)\n                    _arguments \\\n                        '--path[NFS export path]:directory:_directories' \\\n                        '--kubeconfig[Path to kubeconfig file]:file:_files' \\\n                        '--dry-run[Dry-run mode (no changes)]' \\\n                        '--force[Force cleanup without confirmation]' \\\n                        '--help[Show help]'\n                    ;;\n                ui)\n                    _arguments \\\n                        '--path[NFS export path]:directory:_directories' \\\n                        '--addr[Web UI listen address]:address:(:8080 :3000 :9000)' \\\n                        '--help[Show help]'\n                    ;;\n                audit)\n                    _arguments \\\n                        '--file[Audit log file path]:file:_files' \\\n                        '--action[Filter by action]:action:(CREATE UPDATE DELETE CLEANUP)' \\\n                        '--pv[Filter by PV name]:pv:' \\\n                        '--namespace[Filter by namespace]:namespace:' \\\n                        '--start[Start time (RFC3339)]:start:' \\\n                        '--end[End time (RFC3339)]:end:' \\\n                        '--fails-only[Show only failed operations]' \\\n                        '--format[Output format]:format:(table json text)' \\\n                        '--help[Show help]'\n                    ;;\n            esac\n            ;;\n    esac\n}\n\n_nfs_quota_agent \"$@\"\n"

// FishCompletion contains the fish completion script
const FishCompletion = `# fish completion for nfs-quota-agent

# Disable file completion by default
complete -c nfs-quota-agent -f

# Commands
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a run -d 'Run the quota enforcement agent'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a status -d 'Show quota status and disk usage'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a top -d 'Show top directories by usage'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a report -d 'Generate quota report'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a cleanup -d 'Remove orphaned quotas'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a ui -d 'Start web UI dashboard'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a audit -d 'Query audit logs'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a version -d 'Print version information'
complete -c nfs-quota-agent -n '__fish_use_subcommand' -a help -d 'Show help'

# run command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l kubeconfig -d 'Path to kubeconfig file' -r -F
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l nfs-base-path -d 'Local path where NFS is mounted' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l nfs-server-path -d 'NFS server export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l provisioner-name -d 'Provisioner name' -r -a 'nfs.csi.k8s.io cluster.local/nfs-subdir-external-provisioner'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l process-all-nfs -d 'Process all NFS PVs'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l sync-interval -d 'Sync interval' -r -a '10s 30s 1m 5m'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l metrics-addr -d 'Metrics endpoint address' -r -a ':9090 :8080 :9100'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from run' -l audit-log -d 'Audit log file path' -r -F

# status command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from status' -l path -d 'NFS export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from status' -l all -d 'Show all directories'

# top command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from top' -l path -d 'NFS export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from top' -s n -d 'Number of directories' -r -a '5 10 20 50 100'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from top' -l watch -d 'Watch mode'

# report command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from report' -l path -d 'NFS export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from report' -l format -d 'Output format' -r -a 'table json yaml csv'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from report' -l output -d 'Output file' -r -F

# cleanup command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from cleanup' -l path -d 'NFS export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from cleanup' -l kubeconfig -d 'Path to kubeconfig file' -r -F
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from cleanup' -l dry-run -d 'Dry-run mode'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from cleanup' -l force -d 'Force cleanup'

# ui command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from ui' -l path -d 'NFS export path' -r -a '(__fish_complete_directories)'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from ui' -l addr -d 'Listen address' -r -a ':8080 :3000 :9000'

# audit command options
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l file -d 'Audit log file' -r -F
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l action -d 'Filter by action' -r -a 'CREATE UPDATE DELETE CLEANUP'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l pv -d 'Filter by PV name' -r
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l namespace -d 'Filter by namespace' -r
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l start -d 'Start time (RFC3339)' -r
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l end -d 'End time (RFC3339)' -r
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l fails-only -d 'Show only failures'
complete -c nfs-quota-agent -n '__fish_seen_subcommand_from audit' -l format -d 'Output format' -r -a 'table json text'
`

// RunCompletion outputs shell completion script
func RunCompletion(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: nfs-quota-agent completion <shell>")
		fmt.Println("\nSupported shells:")
		fmt.Println("  bash    Generate bash completion script")
		fmt.Println("  zsh     Generate zsh completion script")
		fmt.Println("  fish    Generate fish completion script")
		fmt.Println("\nExamples:")
		fmt.Println("  # Bash (add to ~/.bashrc)")
		fmt.Println("  source <(nfs-quota-agent completion bash)")
		fmt.Println("")
		fmt.Println("  # Zsh (add to ~/.zshrc)")
		fmt.Println("  source <(nfs-quota-agent completion zsh)")
		fmt.Println("")
		fmt.Println("  # Fish")
		fmt.Println("  nfs-quota-agent completion fish | source")
		fmt.Println("")
		fmt.Println("  # Or install permanently:")
		fmt.Println("  # Bash")
		fmt.Println("  nfs-quota-agent completion bash > /etc/bash_completion.d/nfs-quota-agent")
		fmt.Println("")
		fmt.Println("  # Zsh")
		fmt.Println(`  nfs-quota-agent completion zsh > "${fpath[1]}/_nfs-quota-agent"`)
		fmt.Println("")
		fmt.Println("  # Fish")
		fmt.Println("  nfs-quota-agent completion fish > ~/.config/fish/completions/nfs-quota-agent.fish")
		return
	}

	switch args[0] {
	case "bash":
		fmt.Print(BashCompletion)
	case "zsh":
		fmt.Print(ZshCompletion)
	case "fish":
		fmt.Print(FishCompletion)
	default:
		fmt.Fprintf(os.Stderr, "Unknown shell: %s\n", args[0])
		fmt.Fprintf(os.Stderr, "Supported shells: bash, zsh, fish\n")
		os.Exit(1)
	}
}
