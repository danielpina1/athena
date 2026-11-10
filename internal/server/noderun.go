package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/danielpina1/athena/internal/metrics"
	"github.com/danielpina1/athena/internal/node"
	"github.com/danielpina1/athena/internal/nodelink"
)

// NodeRunConfig configures `athena node run`: a node on another machine, linked to the server it joined.
type NodeRunConfig struct {
	// StateDir is the node's state directory (default ~/.athena-node): its identity from `athena node join`, its
	// outbox, its socket, its logs, its work and its own config.json.
	StateDir string
	// Node carries the flags `athena server` also takes (Slots, Model, Blocked, ApprovalTimeout, ClaudeBinary);
	// RunNode fills in the rest.
	Node node.Config
	// CodexBinary replaces the configuration's runtimes.codex.binary; empty keeps it.
	CodexBinary string
	// Stderr receives a copy of the log besides <StateDir>/logs/athena.log (default os.Stderr).
	Stderr  io.Writer
	Version string
}

// RunNode runs a joined node until ctx is done. Nothing of the server's runs here: the node's Engine, credentials,
// Policy and prompts are the server's, over the link (internal/nodelink). The node's config.json, when there is one,
// gives its runtimes and its sandbox; its github, web and router parts are the server's, and ignored.
//
// Wiring order (nodelink.Client): the Client, then the node with the Client's proxies, then Attach, then the link
// runs; the node's socket, which its sessions' hooks and MCP servers reach, is served only once the server gave its
// prompts (Ready), so no session runs with anything but the server's prompts.
func RunNode(ctx context.Context, cfg NodeRunConfig) (err error) {
	if cfg.StateDir == "" {
		return errors.New("node: StateDir is required")
	}
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("node: resolve state dir: %w", err)
	}
	// First: an unjoined directory gets nothing written into it.
	id, err := nodelink.LoadIdentity(stateDir)
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(stateDir); err != nil {
		return err
	}
	sock := filepath.Join(stateDir, NodeSocket)
	if err := claimSocket(sock); err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return fmt.Errorf("athena node run already runs on %s", stateDir)
		}
		return err
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	logger, closeLog, err := openLog(stateDir, stderr)
	if err != nil {
		return err
	}
	defer closeLog()
	prevLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prevLogger)

	fileCfg, err := LoadConfig(stateDir)
	if err != nil {
		return err
	}
	if fileCfg.GitHub != nil || fileCfg.Web != nil {
		logger.Warn("config.json's github and web parts are the server's; a node ignores them")
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	ncfg := cfg.Node
	ncfg.Name = id.Name
	ncfg.StateDir = stateDir
	ncfg.SocketPath = sock
	ncfg.Logger = logger
	ncfg.Metrics = metrics.NewRegistry()
	if ncfg.WorkRoot == "" {
		ncfg.WorkRoot = filepath.Join(stateDir, "work")
	}
	if ncfg.WorkRoot, err = filepath.Abs(ncfg.WorkRoot); err != nil {
		return fmt.Errorf("node: resolve work root: %w", err)
	}
	if ncfg.AthenaBin == "" {
		if ncfg.AthenaBin, err = executable(); err != nil {
			return err
		}
	}
	if ncfg.Sandbox == nil {
		ncfg.Sandbox = fileCfg.nodeSandbox()
	}
	if ncfg.Sandbox == nil {
		logger.Warn("claude code sandbox is off for workers: their commands can write anywhere this user can")
	}
	if fileCfg.Runtimes.Codex.On() {
		if err := checkCodexApprovalTimeout(ncfg.ApprovalTimeout); err != nil {
			return err
		}
		// This machine's own Codex login: <StateDir>/codex (athena codex login --state-dir <StateDir>).
		cx := CodexRuntime(stateDir, fileCfg, ncfg.AthenaBin, cfg.CodexBinary)
		cx.WorkRoot, cx.Version = ncfg.WorkRoot, version
		runtimes := node.DefaultRuntimes(ncfg)
		for name, r := range ncfg.Runtimes {
			runtimes[name] = r
		}
		runtimes[cx.Name()] = cx
		ncfg.Runtimes = runtimes
	}
	if ncfg.Chooser == nil {
		ncfg.Chooser = fileCfg.Runtimes.chooser()
	}

	client, err := nodelink.New(id, nodelink.Options{StateDir: stateDir, Version: version, Log: logger})
	if err != nil {
		return err
	}
	ncfg.Credentials = client.Credentials()
	ncfg.Policy = client.Policy()
	ncfg.WorkerPrompt = client.WorkerPrompt
	ncfg.ReviewerPrompt = client.ReviewerPrompt
	onStatus, onCheckpoint := ncfg.OnStatus, ncfg.OnCheckpoint
	ncfg.OnStatus = func() {
		if onStatus != nil {
			onStatus()
		}
		client.StatusChanged()
	}
	ncfg.OnCheckpoint = func(ticketID, repo, ref, sha string) {
		if onCheckpoint != nil {
			onCheckpoint(ticketID, repo, ref, sha)
		}
		client.Checkpointed(ticketID, repo, ref, sha)
	}
	n := node.New(ncfg, client.Engine())
	client.Attach(n)

	// The link outlives the node: the facts of its sessions' last moments go to the outbox, and out while it lasts.
	linkCtx, linkCancel := context.WithCancel(context.Background())
	linked := make(chan struct{})
	go func() {
		defer close(linked)
		_ = client.Run(linkCtx)
	}()
	defer func() {
		linkCancel()
		<-linked
		if cerr := client.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	defer n.Close()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		n.WatchRuntimes(nodeCtx, time.Minute)
	}()
	defer func() { nodeCancel(); <-watched }()
	logger.Info("athena node starting", "node", id.Name, "id", id.NodeID, "server", id.NodeURL, "stateDir", stateDir,
		"runtimes", slices.Sorted(maps.Keys(ncfg.Runtimes)))

	select {
	case <-client.Ready():
	case <-ctx.Done():
		logger.Info("athena node stopping before it reached the server")
		return nil
	}
	served := make(chan error, 1)
	go func() { served <- n.Serve(nodeCtx) }()
	nodeDone := false
	defer func() {
		// The sessions end first (their ends go to the outbox), then the socket closes.
		n.Close()
		nodeCancel()
		if !nodeDone {
			if serr := <-served; serr != nil && err == nil {
				err = serr
			}
		}
	}()
	if err := waitNodeSocket(sock, served, &nodeDone); err != nil {
		return err
	}
	logger.Info("athena node running", "node", id.Name)
	select {
	case <-ctx.Done():
		logger.Info("athena node stopping")
		return nil
	case serr := <-served:
		nodeDone = true
		return fmt.Errorf("node: socket stopped: %v", serr)
	}
}
