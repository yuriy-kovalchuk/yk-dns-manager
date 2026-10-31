package app

import (
	"flag"
	"os"
	"time"
)

// options holds the command-line flags.
type options struct {
	logLevel           string
	configPath         string
	leaderElect        bool
	requireAccepted    bool
	cleanupGracePeriod time.Duration
	resyncPeriod       time.Duration
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.logLevel, "zap-log-level", os.Getenv("LOG_LEVEL"), "log level: debug, info, warn, error, or a numeric logr verbosity such as -3")
	flag.StringVar(&o.configPath, "config", os.Getenv("CONFIG_PATH"), "path to the config YAML file: DNS provider and domain map")
	flag.BoolVar(&o.leaderElect, "leader-elect", true, "elect a leader so only one replica manages DNS at a time; turn off only when running a single instance outside a cluster")
	flag.BoolVar(&o.requireAccepted, "require-accepted-routes", false, "only publish DNS for HTTPRoutes that at least one parent Gateway has accepted")
	flag.DurationVar(&o.cleanupGracePeriod, "cleanup-grace-period", 5*time.Minute, "how long a failing DNS cleanup of a deleted HTTPRoute is retried before the route is released anyway")
	flag.DurationVar(&o.resyncPeriod, "resync-period", time.Hour, "how often every HTTPRoute is reconciled again even when nothing changed in the cluster, which is what republishes a record someone changed or deleted on the DNS backend; 0 turns it off")
	flag.Parse()
	return o
}
