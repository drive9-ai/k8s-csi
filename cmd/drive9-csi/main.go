package main

import (
	"flag"
	"log"
	"os"

	"github.com/drive9-ai/csi/internal/driver"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	var cfg driver.Config
	flag.StringVar(&cfg.Endpoint, "endpoint", envOr("CSI_ENDPOINT", "unix:///csi/csi.sock"), "CSI endpoint, for example unix:///csi/csi.sock")
	flag.StringVar(&cfg.NodeID, "node-id", envOr("NODE_ID", ""), "Kubernetes node ID")
	flag.StringVar(&cfg.DriverName, "driver-name", envOr("DRIVER_NAME", "csi.drive9.ai"), "CSI driver name")
	flag.StringVar(&cfg.Version, "version", envOr("DRIVER_VERSION", "0.1.0"), "driver version")
	flag.StringVar(&cfg.StateDir, "state-dir", envOr("DRIVE9_CSI_STATE_DIR", "/var/lib/drive9-csi"), "state directory")
	flag.StringVar(&cfg.Drive9Binary, "drive9-binary", envOr("DRIVE9_BINARY", "drive9"), "drive9 CLI binary path")
	flag.Parse()

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("drive9-csi: build in-cluster config: %v", err)
		os.Exit(1)
	}
	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Printf("drive9-csi: create kubernetes client: %v", err)
		os.Exit(1)
	}

	if err := driver.Run(cfg, k8sClient); err != nil {
		log.Printf("drive9-csi: %v", err)
		os.Exit(1)
	}
}

func envOr(key string, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
