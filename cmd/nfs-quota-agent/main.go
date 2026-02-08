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

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// version is set via ldflags at build time
var version = "dev"

func printUsage() {
	fmt.Printf(`nfs-quota-agent %s - NFS Quota Management for Kubernetes

Usage:
  nfs-quota-agent [command] [flags]

Commands:
  run          Run the quota agent (default if no command specified)
  status       Show quota status and disk usage
  top          Show top directories by usage
  report       Generate quota report (JSON/YAML)
  cleanup      Remove orphaned quotas (no matching PV)
  ui           Start web UI dashboard
  audit        Query audit logs
  completion   Generate shell completion script
  version      Print version information

Run 'nfs-quota-agent <command> --help' for more information on a command.

Examples:
  # Run agent in cluster
  nfs-quota-agent run --nfs-base-path=/export --provisioner-name=nfs.csi.k8s.io

  # Show quota status
  nfs-quota-agent status --path=/data

  # Show top 10 directories by usage
  nfs-quota-agent top --path=/data -n 10

  # Generate JSON report
  nfs-quota-agent report --path=/data --format=json

  # Cleanup orphaned quotas (dry-run)
  nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config

  # Cleanup orphaned quotas (force)
  nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --force

  # Start web UI
  nfs-quota-agent ui --path=/data --addr=:8080

  # Query audit logs
  nfs-quota-agent audit --file=/var/log/nfs-quota-agent/audit.log

  # Generate shell completion
  source <(nfs-quota-agent completion bash)
`, version)
}

func main() {
	if len(os.Args) < 2 {
		runAgent(os.Args[1:])
		return
	}

	switch os.Args[1] {
	case "run":
		runAgent(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "top":
		runTop(os.Args[2:])
	case "report":
		runReport(os.Args[2:])
	case "cleanup":
		runCleanup(os.Args[2:])
	case "ui":
		runUI(os.Args[2:])
	case "audit":
		runAudit(os.Args[2:])
	case "completion":
		runCompletion(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("nfs-quota-agent version %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		// If not a known command, assume it's a flag for 'run'
		if os.Args[1][0] == '-' {
			runAgent(os.Args[1:])
		} else {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
	}
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)

	var (
		kubeconfig      string
		nfsBasePath     string
		nfsServerPath   string
		provisionerName string
		processAllNFS   bool
		syncInterval    time.Duration
		metricsAddr     string
		enableUI        bool
		uiAddr          string
		enableAudit     bool
		auditLogPath    string

		// Auto-cleanup options
		enableAutoCleanup  bool
		cleanupInterval    time.Duration
		orphanGracePeriod  time.Duration
		cleanupDryRun      bool

		// History options
		enableHistory     bool
		historyPath       string
		historyInterval   time.Duration
		historyRetention  time.Duration

		// Policy options
		enablePolicy    bool
		defaultQuota    string
		enforceMaxQuota bool
	)

	fs.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (optional, uses in-cluster config if not set)")
	fs.StringVar(&nfsBasePath, "nfs-base-path", "/export", "Local path where NFS is mounted")
	fs.StringVar(&nfsServerPath, "nfs-server-path", "/data", "NFS server's export path")
	fs.StringVar(&provisionerName, "provisioner-name", "cluster.local/nfs-subdir-external-provisioner", "Provisioner name to filter PVs: nfs.csi.k8s.io (csi-driver-nfs) or cluster.local/nfs-subdir-external-provisioner (legacy)")
	fs.BoolVar(&processAllNFS, "process-all-nfs", false, "Process all NFS PVs regardless of provisioner")
	fs.DurationVar(&syncInterval, "sync-interval", 30*time.Second, "Interval between quota syncs")
	fs.StringVar(&metricsAddr, "metrics-addr", ":9090", "Address for Prometheus metrics endpoint")
	fs.BoolVar(&enableUI, "enable-ui", false, "Enable web UI dashboard")
	fs.StringVar(&uiAddr, "ui-addr", ":8080", "Web UI listen address")
	fs.BoolVar(&enableAudit, "enable-audit", false, "Enable audit logging")
	fs.StringVar(&auditLogPath, "audit-log-path", "/var/log/nfs-quota-agent/audit.log", "Audit log file path")

	// Auto-cleanup flags
	fs.BoolVar(&enableAutoCleanup, "enable-auto-cleanup", false, "Enable automatic orphan directory cleanup")
	fs.DurationVar(&cleanupInterval, "cleanup-interval", 1*time.Hour, "Interval between cleanup runs")
	fs.DurationVar(&orphanGracePeriod, "orphan-grace-period", 24*time.Hour, "Grace period before deleting orphans")
	fs.BoolVar(&cleanupDryRun, "cleanup-dry-run", true, "Dry-run mode for cleanup (no actual deletion)")

	// History flags
	fs.BoolVar(&enableHistory, "enable-history", false, "Enable usage history collection")
	fs.StringVar(&historyPath, "history-path", "/var/lib/nfs-quota-agent/history.json", "Path to store usage history")
	fs.DurationVar(&historyInterval, "history-interval", 5*time.Minute, "Interval between history snapshots")
	fs.DurationVar(&historyRetention, "history-retention", 30*24*time.Hour, "How long to keep history data")

	// Policy flags
	fs.BoolVar(&enablePolicy, "enable-policy", false, "Enable namespace quota policy")
	fs.StringVar(&defaultQuota, "default-quota", "1Gi", "Global default quota for namespaces without annotation")
	fs.BoolVar(&enforceMaxQuota, "enforce-max-quota", false, "Enforce maximum quota from namespace annotation")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent run [flags]")
		fmt.Println("\nRun the quota enforcement agent")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
	}

	_ = fs.Parse(args)

	// Create Kubernetes client
	var config *rest.Config
	var err error

	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		slog.Error("Failed to create Kubernetes config", "error", err)
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		slog.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	// Create and run agent
	agent := NewQuotaAgent(client, nfsBasePath, nfsServerPath, provisionerName)
	agent.processAllNFS = processAllNFS
	agent.syncInterval = syncInterval

	// Configure auto-cleanup
	agent.enableAutoCleanup = enableAutoCleanup
	agent.cleanupInterval = cleanupInterval
	agent.orphanGracePeriod = orphanGracePeriod
	agent.cleanupDryRun = cleanupDryRun

	// Configure history
	if enableHistory {
		historyStore, err := NewHistoryStore(historyPath, historyInterval, historyRetention)
		if err != nil {
			slog.Error("Failed to create history store", "error", err)
		} else {
			agent.historyStore = historyStore
			slog.Info("History collection enabled", "path", historyPath, "interval", historyInterval)
		}
	}

	// Configure policy
	agent.enablePolicy = enablePolicy
	if defaultQuota != "" {
		if bytes, err := parseQuotaSize(defaultQuota); err == nil {
			agent.defaultQuota = bytes
		} else {
			slog.Warn("Invalid default-quota value", "value", defaultQuota, "error", err)
		}
	}
	agent.enforceMaxQuota = enforceMaxQuota

	// Initialize audit logger if enabled
	if enableAudit {
		auditConfig := AuditConfig{
			Enabled:  true,
			FilePath: auditLogPath,
		}
		auditLogger, err := NewAuditLogger(auditConfig)
		if err != nil {
			slog.Error("Failed to create audit logger", "error", err)
			os.Exit(1)
		}
		agent.auditLogger = auditLogger
		defer auditLogger.Close()
		slog.Info("Audit logging enabled", "path", auditLogPath)
	}

	// Start metrics server if address is set
	if metricsAddr != "" {
		go startMetricsServer(metricsAddr, agent)
	}

	// Start UI server if enabled
	if enableUI {
		// Only pass audit log path if audit is enabled
		actualAuditPath := ""
		if enableAudit {
			actualAuditPath = auditLogPath
		}
		go func() {
			slog.Info("Starting Web UI", "addr", uiAddr)
			if err := StartUIServerWithAgent(uiAddr, nfsBasePath, nfsServerPath, actualAuditPath, client, agent, agent.historyStore); err != nil {
				slog.Error("Web UI server failed", "error", err)
			}
		}()
	}

	// Handle signals
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := agent.Run(ctx); err != nil {
		slog.Error("Agent failed", "error", err)
		os.Exit(1)
	}
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)

	var (
		path    string
		showAll bool
	)

	fs.StringVar(&path, "path", "/data", "NFS export path to check")
	fs.BoolVar(&showAll, "all", false, "Show all directories (default: top 20)")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent status [flags]")
		fmt.Println("\nShow quota status and disk usage")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
	}

	_ = fs.Parse(args)

	if err := ShowStatus(path, showAll); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runTop(args []string) {
	fs := flag.NewFlagSet("top", flag.ExitOnError)

	var (
		path  string
		count int
		watch bool
	)

	fs.StringVar(&path, "path", "/data", "NFS export path to check")
	fs.IntVar(&count, "n", 10, "Number of top directories to show")
	fs.BoolVar(&watch, "watch", false, "Watch mode (refresh every 5s)")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent top [flags]")
		fmt.Println("\nShow top directories by disk usage")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
	}

	_ = fs.Parse(args)

	if err := ShowTop(path, count, watch); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)

	var (
		path   string
		format string
		output string
	)

	fs.StringVar(&path, "path", "/data", "NFS export path to check")
	fs.StringVar(&format, "format", "table", "Output format: table, json, yaml, csv")
	fs.StringVar(&output, "output", "", "Output file (default: stdout)")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent report [flags]")
		fmt.Println("\nGenerate quota report")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
	}

	_ = fs.Parse(args)

	if err := GenerateReport(path, format, output); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)

	var (
		path         string
		addr         string
		auditLogPath string
	)

	fs.StringVar(&path, "path", "/data", "NFS export path")
	fs.StringVar(&addr, "addr", ":8080", "Web UI listen address")
	fs.StringVar(&auditLogPath, "audit-log", "/var/log/nfs-quota-agent/audit.log", "Audit log file path")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent ui [flags]")
		fmt.Println("\nStart web UI dashboard for monitoring quotas and audit logs")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
		fmt.Println("\nThe UI will be available at http://localhost:8080 (or your specified address)")
	}

	_ = fs.Parse(args)

	fmt.Printf("Starting NFS Quota Web UI...\n")
	fmt.Printf("Path: %s\n", path)
	fmt.Printf("Audit: %s\n", auditLogPath)
	fmt.Printf("URL:  http://localhost%s\n\n", addr)

	if err := StartUIServerFull(addr, path, path, auditLogPath, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runCleanup(args []string) {
	fs := flag.NewFlagSet("cleanup", flag.ExitOnError)

	var (
		path       string
		kubeconfig string
		dryRun     bool
		force      bool
	)

	fs.StringVar(&path, "path", "/data", "NFS export path")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	fs.BoolVar(&dryRun, "dry-run", true, "Dry-run mode (no changes)")
	fs.BoolVar(&force, "force", false, "Force cleanup without confirmation")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent cleanup [flags]")
		fmt.Println("\nRemove orphaned quotas that have no matching PV in Kubernetes")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
		fmt.Println("\nExamples:")
		fmt.Println("  # Dry-run (default, shows what would be removed)")
		fmt.Println("  nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config")
		fmt.Println("")
		fmt.Println("  # Actually remove orphaned quotas")
		fmt.Println("  nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false")
		fmt.Println("")
		fmt.Println("  # Force remove without confirmation")
		fmt.Println("  nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false --force")
	}

	_ = fs.Parse(args)

	if err := RunCleanup(path, kubeconfig, dryRun, force); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)

	var (
		filePath  string
		action    string
		pvName    string
		namespace string
		startTime string
		endTime   string
		failsOnly bool
		format    string
		limit     int
	)

	fs.StringVar(&filePath, "file", "/var/log/nfs-quota-agent/audit.log", "Audit log file path")
	fs.StringVar(&action, "action", "", "Filter by action (CREATE, UPDATE, DELETE, CLEANUP)")
	fs.StringVar(&pvName, "pv", "", "Filter by PV name")
	fs.StringVar(&namespace, "namespace", "", "Filter by namespace")
	fs.StringVar(&startTime, "start", "", "Start time (RFC3339 format)")
	fs.StringVar(&endTime, "end", "", "End time (RFC3339 format)")
	fs.BoolVar(&failsOnly, "fails-only", false, "Show only failed operations")
	fs.StringVar(&format, "format", "table", "Output format: table, json, text")
	fs.IntVar(&limit, "limit", 100, "Maximum number of entries to show")

	fs.Usage = func() {
		fmt.Println("Usage: nfs-quota-agent audit [flags]")
		fmt.Println("\nQuery and display audit logs")
		fmt.Println("\nFlags:")
		fs.PrintDefaults()
		fmt.Println("\nExamples:")
		fmt.Println("  # Show recent audit entries")
		fmt.Println("  nfs-quota-agent audit --file=/var/log/nfs-quota-agent/audit.log")
		fmt.Println("")
		fmt.Println("  # Show only failed operations")
		fmt.Println("  nfs-quota-agent audit --fails-only")
		fmt.Println("")
		fmt.Println("  # Filter by action type")
		fmt.Println("  nfs-quota-agent audit --action=CREATE")
		fmt.Println("")
		fmt.Println("  # Output as JSON")
		fmt.Println("  nfs-quota-agent audit --format=json")
	}

	_ = fs.Parse(args)

	// Build filter
	filter := AuditFilter{
		Action:    AuditAction(action),
		PVName:    pvName,
		Namespace: namespace,
		OnlyFails: failsOnly,
	}

	if startTime != "" {
		if t, err := time.Parse(time.RFC3339, startTime); err == nil {
			filter.StartTime = t
		} else {
			fmt.Fprintf(os.Stderr, "Invalid start time format: %s\n", startTime)
			os.Exit(1)
		}
	}

	if endTime != "" {
		if t, err := time.Parse(time.RFC3339, endTime); err == nil {
			filter.EndTime = t
		} else {
			fmt.Fprintf(os.Stderr, "Invalid end time format: %s\n", endTime)
			os.Exit(1)
		}
	}

	// Query audit log
	entries, err := QueryAuditLog(filePath, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading audit log: %v\n", err)
		os.Exit(1)
	}

	// Apply limit
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}

	if len(entries) == 0 {
		fmt.Println("No audit entries found matching the filter.")
		return
	}

	fmt.Printf("Found %d audit entries:\n\n", len(entries))
	PrintAuditEntries(entries, format)
}
