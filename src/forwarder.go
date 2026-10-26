package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	forwardedFlag      = "$Forwarded"
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 60 * time.Second
	pollInterval       = 30 * time.Second
)

// ForwarderStatus describes the current state of a forwarder.
type ForwarderStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	LastSync  string `json:"lastSync,omitempty"`
	Error     string `json:"error,omitempty"`
}

// IMAPClient is the subset of imapclient.Client used by the forwarder.
type IMAPClient interface {
	Login(username, password string) *imapclient.Command
	Logout() *imapclient.Command
	Close() error
	Closed() <-chan struct{}
	Select(mailbox string, options *imap.SelectOptions) *imapclient.SelectCommand
	UIDSearch(criteria *imap.SearchCriteria, options *imap.SearchOptions) *imapclient.SearchCommand
	Fetch(numSet imap.NumSet, options *imap.FetchOptions) *imapclient.FetchCommand
	Store(numSet imap.NumSet, store *imap.StoreFlags, options *imap.StoreOptions) *imapclient.FetchCommand
	Idle() (*imapclient.IdleCommand, error)
	List(ref string, pattern string, options *imap.ListOptions) *imapclient.ListCommand
	Append(mailbox string, size int64, options *imap.AppendOptions) *imapclient.AppendCommand
	Create(mailbox string, options *imap.CreateOptions) *imapclient.Command
	Expunge() *imapclient.ExpungeCommand
}

// IMAPDialFunc creates a new IMAP client connection.
type IMAPDialFunc func(host string, port int, secure *bool) (IMAPClient, error)

// DefaultIMAPDial connects to an IMAP server using the appropriate TLS mode.
func DefaultIMAPDial(host string, port int, secure *bool) (IMAPClient, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	if secure != nil && *secure {
		return imapclient.DialTLS(addr, nil)
	}
	return imapclient.DialStartTLS(addr, nil)
}

// Forwarder monitors a source IMAP account and forwards new messages.
type Forwarder struct {
	source SourceConfig
	sender Sender
	state  *StateStore // nil unless the source uses local tracking
	logger *Logger

	// UIDs forwarded by this process per folder (local tracking backstop).
	sentMu sync.Mutex
	sent   map[string]map[imap.UID]struct{}

	mu        sync.Mutex
	connected bool
	lastSync  string
	lastErr   string

	onStatusChange func(ForwarderStatus)
}

// NewForwarder creates a new Forwarder for the given source.
func NewForwarder(
	source SourceConfig,
	sender Sender,
	state *StateStore,
	onStatusChange func(ForwarderStatus),
) *Forwarder {
	return &Forwarder{
		source:         source,
		sender:         sender,
		state:          state,
		logger:         newLogger(source.Name),
		onStatusChange: onStatusChange,
	}
}

// GetStatus returns the current status of this forwarder.
func (f *Forwarder) GetStatus() ForwarderStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ForwarderStatus{
		Name:      f.source.Name,
		Connected: f.connected,
		LastSync:  f.lastSync,
		Error:     f.lastErr,
	}
}

func (f *Forwarder) setConnected(v bool) {
	f.mu.Lock()
	f.connected = v
	f.mu.Unlock()
}

func (f *Forwarder) setError(err string) {
	f.mu.Lock()
	f.lastErr = err
	f.mu.Unlock()
}

func (f *Forwarder) setLastSync() {
	f.mu.Lock()
	f.lastSync = time.Now().UTC().Format(time.RFC3339)
	f.mu.Unlock()
}

// Run starts the forwarding loop. It blocks until ctx is cancelled.
func (f *Forwarder) Run(ctx context.Context) {
	if f.source.LocalTracking && f.state == nil {
		msg := "local tracking is enabled but no state store is available"
		f.setError(msg)
		f.notifyStatus()
		f.logger.Error("%s", msg)
		return
	}

	delay := reconnectBaseDelay
	for {
		err := f.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			f.setError(err.Error())
			f.setConnected(false)
			f.notifyStatus()
			f.logger.Error("Connection error: %v", err)
		}

		f.logger.Info("Reconnecting in %v...", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// Stop cleans up sender resources.
func (f *Forwarder) Stop() {
	if f.sender != nil {
		if err := f.sender.Close(); err != nil {
			f.logger.Warn("Error closing sender: %v", err)
		}
	}
	f.setConnected(false)
	f.notifyStatus()
	f.logger.Info("Stopped")
}

func (f *Forwarder) notifyStatus() {
	if f.onStatusChange != nil {
		f.onStatusChange(f.GetStatus())
	}
}

// runOnce opens one IMAP connection per folder and monitors them concurrently.
// IMAP only allows one selected mailbox per connection, so each folder needs
// its own connection for concurrent IDLE monitoring.
func (f *Forwarder) runOnce(ctx context.Context) error {
	if len(f.source.Folders) == 1 {
		return f.monitorFolder(ctx, f.source.Folders[0], true)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i, folder := range f.source.Folders {
		wg.Add(1)
		go func(folder string, first bool) {
			defer wg.Done()
			if err := f.monitorFolder(runCtx, folder, first); err != nil && runCtx.Err() == nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(folder, i == 0)
	}

	wg.Wait()
	return firstErr
}

func (f *Forwarder) connectSource(folder string) (*imapclient.Client, chan struct{}, error) {
	newMail := make(chan struct{}, 1)
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					select {
					case newMail <- struct{}{}:
					default:
					}
				}
			},
		},
	}

	addr := fmt.Sprintf("%s:%d", f.source.Host, f.source.Port)
	var client *imapclient.Client
	var err error
	if f.source.Secure != nil && *f.source.Secure {
		client, err = imapclient.DialTLS(addr, opts)
	} else {
		client, err = imapclient.DialStartTLS(addr, opts)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	if err := client.Login(f.source.Auth.User, f.source.Auth.Pass).Wait(); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("login: %w", err)
	}

	return client, newMail, nil
}

// monitorFolder connects, selects a folder, forwards existing messages, and
// watches for new ones via IDLE. listFolders is only done on the first folder.
func (f *Forwarder) monitorFolder(ctx context.Context, folder string, listFolders bool) error {
	client, newMail, err := f.connectSource(folder)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	f.logger.Info("Connected to %s:%d for folder %s", f.source.Host, f.source.Port, folder)
	f.setConnected(true)
	f.setError("")
	f.notifyStatus()

	if listFolders {
		f.listAvailableFolders(client)
	}

	// Select the folder
	selectData, err := client.Select(folder, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %q: %w", folder, err)
	}

	// Recorded UIDs are only valid for the UIDVALIDITY they were seen under.
	if f.source.LocalTracking {
		reset, err := f.state.SyncUIDValidity(f.source.TrackingID, folder, selectData.UIDValidity)
		if err != nil {
			return fmt.Errorf("sync UIDVALIDITY for %q: %w", folder, err)
		}
		if reset {
			f.logger.Warn("UIDVALIDITY of %q changed; forgetting previously forwarded messages in this folder", folder)
		}
	}

	// Forward any existing unforwarded messages
	if err := f.forwardNewMessages(ctx, client, folder); err != nil {
		f.logger.Error("Error forwarding messages in %q: %v", folder, err)
	}

	// Watch for new messages via IDLE, or fall back to polling when the server
	// doesn't advertise IDLE support.
	return f.watchFolder(ctx, client, folder, newMail)
}

func (f *Forwarder) listAvailableFolders(client *imapclient.Client) {
	listCmd := client.List("", "*", nil)
	mailboxes, err := listCmd.Collect()
	if err != nil {
		f.logger.Warn("Failed to list folders: %v", err)
		return
	}
	names := make([]string, len(mailboxes))
	for i, m := range mailboxes {
		names[i] = m.Mailbox
	}
	f.logger.Info("Available folders: %v", names)
}

func (f *Forwarder) watchFolder(ctx context.Context, client *imapclient.Client, folder string, newMail <-chan struct{}) error {
	if !supportsIdle(client.Caps()) {
		f.logger.Info("Server does not support IDLE, falling back to NOOP polling every %v for %s", pollInterval, folder)
		return f.pollFolder(ctx, client, folder)
	}

	f.logger.Info("Watching for new messages in %s...", folder)

	for {
		if ctx.Err() != nil {
			return nil
		}

		idleCmd, err := client.Idle()
		if err != nil {
			if shouldFallbackFromIdleError(err) {
				f.logger.Warn("IDLE rejected by server, falling back to NOOP polling for %s: %v", folder, err)
				return f.pollFolder(ctx, client, folder)
			}
			return fmt.Errorf("idle: %w", err)
		}

		// Wait for context cancellation, connection close, or new mail notification
		select {
		case <-ctx.Done():
			_ = idleCmd.Close()
			_ = idleCmd.Wait()
			return nil
		case <-client.Closed():
			_ = idleCmd.Close()
			_ = idleCmd.Wait()
			return fmt.Errorf("connection closed during IDLE")
		case <-newMail:
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("idle close: %w", err)
			}
			if err := idleCmd.Wait(); err != nil {
				return fmt.Errorf("idle wait: %w", err)
			}
		}

		// Drain any queued notifications
		select {
		case <-newMail:
		default:
		}

		f.logger.Info("New message(s) detected in %s", folder)
		if err := f.forwardNewMessages(ctx, client, folder); err != nil {
			f.logger.Error("Error forwarding messages in %q: %v", folder, err)
		}
	}
}

func (f *Forwarder) pollFolder(ctx context.Context, client *imapclient.Client, folder string) error {
	f.logger.Info("Polling for new messages in %s with NOOP every %v...", folder, pollInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-client.Closed():
			return fmt.Errorf("connection closed during polling")
		case <-ticker.C:
			if err := client.Noop().Wait(); err != nil {
				return fmt.Errorf("noop: %w", err)
			}
			if err := f.forwardNewMessages(ctx, client, folder); err != nil {
				f.logger.Error("Error forwarding messages in %q: %v", folder, err)
			}
		}
	}
}

func supportsIdle(caps imap.CapSet) bool {
	return caps.Has(imap.CapIdle)
}

func shouldFallbackFromIdleError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unrecognized command") ||
		strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "command unknown") ||
		strings.Contains(msg, "not supported")
}

// searchCriteria selects the messages that may still need forwarding. With
// local tracking the $Forwarded flag is not used; already forwarded UIDs are
// filtered out against the state store instead.
func searchCriteria(localTracking bool) *imap.SearchCriteria {
	if localTracking {
		return &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	}
	return &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen, imap.Flag(forwardedFlag)},
	}
}

// pendingUIDs drops UIDs already recorded in the state store or forwarded by
// this process (see recordForwarded).
func (f *Forwarder) pendingUIDs(folder string, uids []imap.UID) ([]imap.UID, error) {
	pending, err := f.state.FilterUnforwarded(f.source.TrackingID, folder, uids)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, uid := range pending {
		if !f.wasSent(folder, uid) {
			pending[n] = uid
			n++
		}
	}
	return pending[:n], nil
}

// recordForwarded remembers a forwarded UID. It is kept in memory first so
// that a failing state store cannot make the same message be forwarded again
// on every check; the failure is logged and surfaced in the status instead.
func (f *Forwarder) recordForwarded(folder string, uid imap.UID) {
	f.rememberSent(folder, uid)
	if err := f.state.MarkForwarded(f.source.TrackingID, folder, uid); err != nil {
		f.logger.Error("Message UID %d: failed to record forwarded state (it may be forwarded again after a restart): %v", uid, err)
		f.setError(fmt.Sprintf("state store write failed: %v", err))
		f.notifyStatus()
	}
}

func (f *Forwarder) rememberSent(folder string, uid imap.UID) {
	f.sentMu.Lock()
	defer f.sentMu.Unlock()
	if f.sent == nil {
		f.sent = make(map[string]map[imap.UID]struct{})
	}
	if f.sent[folder] == nil {
		f.sent[folder] = make(map[imap.UID]struct{})
	}
	f.sent[folder][uid] = struct{}{}
}

func (f *Forwarder) wasSent(folder string, uid imap.UID) bool {
	f.sentMu.Lock()
	defer f.sentMu.Unlock()
	_, ok := f.sent[folder][uid]
	return ok
}

func (f *Forwarder) forwardNewMessages(ctx context.Context, client *imapclient.Client, folder string) error {
	searchData, err := client.UIDSearch(searchCriteria(f.source.LocalTracking), nil).Wait()
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	uids := searchData.AllUIDs()
	if f.source.LocalTracking {
		uids, err = f.pendingUIDs(folder, uids)
		if err != nil {
			return fmt.Errorf("filter forwarded: %w", err)
		}
	}
	if len(uids) == 0 {
		return nil
	}

	f.logger.Info("Found %d new message(s) to forward", len(uids))

	for _, uid := range uids {
		if ctx.Err() != nil {
			return nil
		}
		if err := f.forwardMessage(ctx, client, folder, uid); err != nil {
			f.logger.Error("Message UID %d: failed to forward: %v", uid, err)
		}
	}

	return nil
}

func (f *Forwarder) forwardMessage(ctx context.Context, client *imapclient.Client, folder string, uid imap.UID) error {
	fetchOpts := &imap.FetchOptions{
		UID:   true,
		Flags: true,
		BodySection: []*imap.FetchItemBodySection{
			{Peek: true}, // BODY.PEEK[] — full message without marking as \Seen
		},
	}

	uidSet := imap.UIDSetNum(uid)
	fetchCmd := client.Fetch(uidSet, fetchOpts)
	defer func() { _ = fetchCmd.Close() }()

	msg := fetchCmd.Next()
	if msg == nil {
		return fmt.Errorf("no message data returned")
	}

	buf, err := msg.Collect()
	if err != nil {
		return fmt.Errorf("collect message: %w", err)
	}

	// Extract the raw message body. Try the exact section first, then fall
	// back to the first available body section (server may strip the Peek flag
	// from the response tag).
	var rawMessage []byte
	if data := buf.FindBodySection(&imap.FetchItemBodySection{Peek: true}); data != nil {
		rawMessage = data
	} else if len(buf.BodySection) > 0 {
		rawMessage = buf.BodySection[0].Bytes
	}
	if rawMessage == nil {
		return fmt.Errorf("no body section in response")
	}

	if err := f.sender.Send(ctx, rawMessage, f.source.TargetFolder); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	f.logger.Info("Message UID %d: forwarded successfully", uid)

	if f.source.LocalTracking {
		// Remember locally; the source mailbox is left untouched.
		f.recordForwarded(folder, uid)
	} else {
		// Mark as forwarded on source
		storeCmd := client.Store(uidSet, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.Flag(forwardedFlag)},
		}, nil)
		if err := storeCmd.Close(); err != nil {
			f.logger.Warn("Message UID %d: failed to set forwarded flag: %v", uid, err)
		}
	}

	// Optionally delete after forwarding
	if f.source.DeleteAfterForward {
		delCmd := client.Store(uidSet, &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagDeleted},
		}, nil)
		if err := delCmd.Close(); err != nil {
			f.logger.Warn("Message UID %d: failed to mark deleted: %v", uid, err)
		}
		expungeCmd := client.Expunge()
		if err := expungeCmd.Close(); err != nil {
			f.logger.Warn("Message UID %d: expunge error: %v", uid, err)
		}
		f.logger.Info("Message UID %d: deleted from source", uid)
	}

	f.setLastSync()
	f.notifyStatus()
	return nil
}
