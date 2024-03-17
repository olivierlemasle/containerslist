package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/alertmanager/cluster"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultHttpListenAddress = "0.0.0.0:3000"

	defaultClusterAddress     = "0.0.0.0:9094"
	defaultPeerTimeout        = 15 * time.Second
	defaultGossipInterval     = 200 * time.Millisecond
	defaultConfigPollInterval = time.Minute

	tpl = `
<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <title>containerlist</title>
  </head>
  <body>
    <p>Name: {{ .Name }}</p>
    <p>Status: {{ .Status }}</p>
    <p>Peers:</p>
    <ul>
    {{ range .Peers }}
      <li>{{ .Name }}: <code>{{ .Address }}</code></li>
    {{ end }}
    </ul>
  </body>
</html>
`
)

var (
	httpAddr = flag.String("http", defaultHttpListenAddress, "HTTP listen address")

	gossipInterval   = flag.Duration("ha_gossip_interval", defaultGossipInterval, "HA gossip interval")
	pushPullInterval = flag.Duration("ha_push_pull_interval", cluster.DefaultPushPullInterval, "HA push/pull interval")
	listenAddr       = flag.String("ha_listen_address", defaultClusterAddress, "HA listen address")
	advertiseAddr    = flag.String("ha_advertise_address", "", "HA advertise address")
	label            = flag.String("ha_label", "", "HA label")
	peersStr         = flag.String("ha_peers", "", "HA peers")

	peers []string
)

func main() {
	flag.Parse()

	if *peersStr != "" {
		for _, peer := range strings.Split(*peersStr, ",") {
			peer = strings.TrimSpace(peer)
			peers = append(peers, peer)
		}
	}

	t, err := template.New("webpage").Parse(tpl)
	if err != nil {
		panic(err)
	}

	w := log.NewSyncWriter(os.Stderr)
	logger := log.NewLogfmtLogger(w)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer stop()

	m, err := NewManager(logger)
	if err != nil {
		panic(err)
	}
	go m.Run(ctx)

	s := &http.Server{
		Addr:    *httpAddr,
		Handler: m.http(t),
	}

	go func() {
		if err := s.ListenAndServe(); err != nil && errors.Is(err, http.ErrServerClosed) {
			level.Warn(logger).Log("Error: %v", err)
		}
	}()

	<-ctx.Done()
	level.Debug(m.logger).Log("Stopping")
	stop()
}

type Manager struct {
	logger       log.Logger
	peer         cluster.ClusterPeer
	settleCancel context.CancelFunc
}

func NewManager(logger log.Logger) (*Manager, error) {
	m := &Manager{
		logger: logger,
	}

	if err := m.setupClustering(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) setupClustering() error {
	reg := prometheus.NewRegistry()

	peer, err := cluster.Create(m.logger, reg, *listenAddr, *advertiseAddr, peers, true, *pushPullInterval, *gossipInterval, cluster.DefaultTCPTimeout, cluster.DefaultProbeTimeout, cluster.DefaultProbeInterval, nil, true, *label)
	if err != nil {
		return fmt.Errorf("unable to initialize gossip mesh: %w", err)
	}

	err = peer.Join(cluster.DefaultReconnectInterval, cluster.DefaultReconnectTimeout)
	if err != nil {
		level.Error(m.logger).Log("Unable to join gossip mesh while initializing cluster for high availability mode", "error", err)
	}

	// Attempt to verify the number of peers for 30s every 2s.
	var ctx context.Context
	const settleTimeout = cluster.DefaultGossipInterval * 10
	ctx, m.settleCancel = context.WithTimeout(context.Background(), 30*time.Second)
	go peer.Settle(ctx, settleTimeout)
	m.peer = peer
	return nil
}

func (m *Manager) Run(ctx context.Context) error {
	for range ctx.Done() {
		m.StopAndWait()
	}
	return nil
}

func (m *Manager) StopAndWait() {
	m.settleCancel()
}

func (m *Manager) http(t *template.Template) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := t.Execute(w, m.peer); err != nil {
			fmt.Fprintf(w, "Error: %v", err)
		}
	})
}
