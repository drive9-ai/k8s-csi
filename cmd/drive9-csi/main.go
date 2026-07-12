package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/drive9-ai/csi/internal/driver"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Printf("drive9-csi: %v", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "install-host-binaries":
			return runInstallHostBinariesCommand(args[1:], stdout)
		case "supervise-sidecar-mount":
			return runSuperviseSidecarMountCommand(args[1:])
		case "verify-host-binary":
			return runVerifyHostBinaryCommand(args[1:], stdout)
		}
	}
	return runCSI(args)
}

func runCSI(args []string) error {
	var cfg driver.Config
	flags := flag.NewFlagSet("drive9-csi", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.Endpoint, "endpoint", envOr("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI endpoint, for example unix:///csi/csi.sock")
	flags.StringVar(&cfg.NodeID, "node-id", envOr("NODE_ID", ""), "Kubernetes node ID")
	flags.StringVar(&cfg.DriverName, "driver-name", envOr("DRIVER_NAME", "csi.drive9.ai"), "CSI driver name")
	flags.StringVar(&cfg.Version, "version", envOr("DRIVER_VERSION", "0.1.0"), "driver version")
	flags.StringVar(&cfg.StateDir, "state-dir", envOr("DRIVE9_CSI_STATE_DIR", "/var/lib/drive9-csi"), "state directory")
	flags.StringVar(&cfg.Drive9Binary, "drive9-binary", envOr("DRIVE9_BINARY", "drive9"), "drive9 CLI binary path")
	flags.StringVar(&cfg.RecoverNodeMounts, "recover-node-mounts", envOr("DRIVE9_CSI_RECOVER_NODE_MOUNTS", "auto"), "node mount recovery mode: auto, enabled, or disabled")
	flags.StringVar(&cfg.ServiceMode, "service-mode", envOr("DRIVE9_CSI_SERVICE_MODE", "auto"), "CSI service mode: auto, controller, or node")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", flags.Args())
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("build in-cluster config: %w", err)
	}
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	return driver.Run(cfg, k8sClient)
}

func envOr(key string, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
