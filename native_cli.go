package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/kevmo314/wechat-cli/internal/native"
)

type nativeState struct {
	Account *native.Account `json:"account,omitempty"`
	Cursor  []byte          `json:"cursor,omitempty"`
}
type nativeJournal struct {
	Owner    string           `json:"owner"`
	Batch    native.SyncBatch `json:"batch"`
	Messages []native.Message `json:"messages"`
}

// Recover ordering metadata from raw sync commands saved by older versions.
func (j nativeJournal) orderedMessages() []native.Message {
	result := append([]native.Message(nil), j.Messages...)
	indices := map[string]int{}
	for i, m := range result {
		indices[m.ID] = i
	}
	for _, cmd := range j.Batch.Commands {
		if cmd.ID != 5 {
			continue
		}
		m, err := native.DecodeMessage(cmd.Payload, j.Owner)
		if err != nil {
			continue
		}
		if i, ok := indices[m.ID]; ok {
			native.MergeMessageOrder(&result[i], m)
		}
	}
	return result
}

type nativeStore struct{ dir string }

func openNativeStore(s Store) (nativeStore, error) {
	n := nativeStore{filepath.Join(s.Dir, "native")}
	if err := os.MkdirAll(n.dir, 0700); err != nil {
		return n, err
	}
	return n, os.Chmod(n.dir, 0700)
}
func (s nativeStore) load(name string, value any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid native %s", name)
	}
	return nil
}
func (s nativeStore) save(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	f, err := os.CreateTemp(filepath.Dir(path), ".native-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func (s nativeStore) lock(ctx context.Context) (*flock.Flock, error) {
	path := filepath.Join(s.dir, "state.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	l := flock.New(path)
	ok, err := l.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		l.Close()
		return nil, err
	}
	if !ok {
		l.Close()
		return nil, errors.New("native account is busy")
	}
	return l, nil
}
func (s nativeStore) state() (nativeState, error) {
	var result nativeState
	err := s.load("state.json", &result)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil && result.Account != nil {
		err = result.Account.Validate()
	}
	return result, err
}
func (s nativeStore) journal(record nativeJournal) error {
	dir := filepath.Join(s.dir, "batches")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	return s.save(filepath.Join("batches", fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(suffix))), record)
}
func (s nativeStore) history(owner string) ([]native.Message, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "batches", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	seen := map[string]int{}
	var result []native.Message
	for _, path := range files {
		var record nativeJournal
		if err = s.load(filepath.Join("batches", filepath.Base(path)), &record); err != nil {
			return nil, err
		}
		if owner != "" && record.Owner != owner {
			continue
		}
		for _, m := range record.orderedMessages() {
			key := record.Owner + "/" + m.ID
			if i, ok := seen[key]; ok {
				native.MergeMessageOrder(&result[i], m)
			} else {
				seen[key] = len(result)
				result = append(result, m)
			}
		}
	}
	native.ProjectMessages(result)
	native.SortMessages(result)
	return result, nil
}

func nativeCommand(ctx context.Context, store Store, command string, args []string) error {
	s, err := openNativeStore(store)
	if err != nil {
		return err
	}
	switch command {
	case "login":
		return nativeLogin(ctx, store, s, args)
	case "translate":
		return translateCommand(ctx, store, args)
	case "mark-read":
		return markReadCommand(ctx, store, args)
	case "attachment":
		return attachmentCommand(ctx, store, args)
	case "wait":
		return waitMessage(ctx, store, args)
	case "read":
		return nativeRead(ctx, store, s, args)
	case "send":
		return nativeSend(ctx, s, args)
	case "status", "contacts", "history", "logout":
		f := flags(command)
		limit := 50
		if command == "history" {
			f.IntVar(&limit, "limit", 50, "maximum locally archived messages")
		}
		if err = parse(f, args); err != nil {
			return err
		}
		if limit < 1 || limit > 10000 {
			return errors.New("limit must be between 1 and 10000")
		}
		if command == "logout" {
			reader, err := store.readerLock()
			if err != nil {
				return err
			}
			defer reader.Close()
			lock, err := s.lock(ctx)
			if err != nil {
				return err
			}
			defer lock.Close()
			for _, name := range []string{"state.json", "login.json"} {
				if err = os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			fmt.Println("Native local credentials removed; retained archive. The phone's desktop authorization was not revoked.")
			return nil
		}
		state, err := s.state()
		if err != nil {
			return err
		}
		if command == "status" {
			result := map[string]any{"backend": "native", "logged_in": state.Account != nil}
			if state.Account != nil {
				result["username"] = state.Account.Username
				result["profile"] = state.Account.Profile
				result["host"] = state.Account.Host
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		owner := ""
		if state.Account != nil {
			owner = state.Account.Username
		}
		messages, err := s.history(owner)
		if err != nil {
			return err
		}
		if command == "contacts" {
			seen := map[string]bool{}
			for _, m := range messages {
				seen[m.Peer] = true
			}
			keys := make([]string, 0, len(seen))
			for peer := range seen {
				keys = append(keys, peer)
			}
			sort.Strings(keys)
			for _, peer := range keys {
				fmt.Println(peer)
			}
			return nil
		}
		if len(messages) > limit {
			messages = messages[len(messages)-limit:]
		}
		for _, m := range messages {
			if err = json.NewEncoder(os.Stdout).Encode(m); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown native command %q; use wechat --help", command)
	}
}

type nativeLoginOptions struct {
	qr              string
	timeout         time.Duration
	resume          bool
	profile         string
	profileExplicit bool
}

func parseNativeLoginOptions(args []string) (nativeLoginOptions, error) {
	var opts nativeLoginOptions
	f := flags("login")
	f.StringVar(&opts.qr, "qr", "", "also save the native QR as PNG")
	f.DurationVar(&opts.timeout, "timeout", 8*time.Minute, "overall login timeout")
	f.BoolVar(&opts.resume, "resume", false, "resume the saved QR/approval attempt")
	f.StringVar(&opts.profile, "profile", native.DesktopProfile, "phone, tablet, or desktop (use a separate --config-dir)")
	if err := parse(f, args); err != nil {
		return opts, err
	}
	profile, err := native.NormalizeProfile(opts.profile)
	if err != nil {
		return opts, err
	}
	opts.profile = profile
	f.Visit(func(v *flag.Flag) {
		if v.Name == "profile" {
			opts.profileExplicit = true
		}
	})
	return opts, nil
}

func nativeLogin(ctx context.Context, store Store, s nativeStore, args []string) error {
	opts, err := parseNativeLoginOptions(args)
	if err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	reader, err := store.readerLock()
	if err != nil {
		return err
	}
	defer reader.Close()
	lock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Account != nil {
		return errors.New("already logged in natively; use logout or a different --config-dir")
	}
	host := "long.weixin.qq.com"
	var attempt *native.LoginAttempt
	if opts.resume {
		attempt = &native.LoginAttempt{}
		if err = s.load("login.json", attempt); err != nil {
			return err
		}
		host = attempt.Host
		storedProfile := attempt.Profile
		if storedProfile == "" {
			storedProfile = native.DesktopProfile
		}
		if opts.profileExplicit && storedProfile != opts.profile {
			return errors.New("saved login uses a different profile; start a fresh login or use its matching --profile")
		}
		if storedProfile != native.DesktopProfile && storedProfile != native.AndroidTabletProfile && storedProfile != native.PhoneProfile {
			return errors.New("unsupported saved login profile")
		}
		if time.Now().Unix()-attempt.Created > 900 {
			return errors.New("saved QR expired; run login without --resume")
		}
	}
	connection, err := native.Connect(ctx, host)
	if err != nil {
		return err
	}
	defer connection.Close()
	if attempt == nil {
		attempt, err = connection.StartLoginWithProfile(ctx, host, opts.profile)
		if err != nil {
			return err
		}
		if err = s.save("login.json", attempt); err != nil {
			return err
		}
	}
	if attempt.Profile == native.PhoneProfile {
		fmt.Fprintln(os.Stderr, "Experimental phone login may replace your primary phone session.\nApprove on your phone only if you want to test this profile.")
	} else if attempt.Profile == native.AndroidTabletProfile {
		fmt.Fprintln(os.Stderr, "Experimental Android tablet login. Desktop coexistence was observed on the tested account; other device combinations are unverified.\nApprove WeChat CLI on your phone to test it.")
	} else {
		fmt.Fprintln(os.Stderr, "Native desktop login uses your desktop session and can log out another computer.\nApprove WeChat CLI on your phone to continue.")
	}
	if attempt.Status != 2 {
		mime := "image/png"
		if len(attempt.Image) > 0 && attempt.Image[0] == 0xff {
			mime = "image/jpeg"
		}
		content := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(attempt.Image)
		if err = displayLoginQR(ctx, newClient(defaultBaseURL, "").HTTP, os.Stderr, QRCode{Content: content}, opts.qr); err != nil {
			return err
		}
	}
	last := uint32(0)
	for attempt.Status != 2 {
		if err = connection.PollLogin(ctx, attempt); err != nil {
			return err
		}
		if err = s.save("login.json", attempt); err != nil {
			return err
		}
		if attempt.Status != last {
			if attempt.Status == 1 {
				fmt.Fprintln(os.Stderr, "Scanned; confirm login on your phone.")
			}
			last = attempt.Status
		}
		if attempt.Status == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintln(os.Stderr, "Phone approved; authenticating the native account…")
	account, err := connection.Authenticate(ctx, attempt)
	if err != nil {
		return err
	}
	if err = s.save("state.json", nativeState{Account: account}); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(s.dir, "login.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("Native login complete. Run `wechat read` to synchronize messages.")
	return nil
}

func nativeRead(ctx context.Context, store Store, s nativeStore, args []string) error {
	f := flags("read")
	follow := f.Bool("follow", false, "keep synchronizing messages")
	asJSON := f.Bool("json", false, "print decoded messages as JSON")
	if err := parse(f, args); err != nil {
		return err
	}
	reader, err := store.readerLock()
	if err != nil {
		return err
	}
	defer reader.Close()
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Account == nil {
		return errors.New("not logged in natively; run wechat login")
	}
	connection, err := native.Connect(ctx, state.Account.Host)
	if err != nil {
		return err
	}
	defer connection.Close()
	seen := map[string]bool{}
	history, err := s.history(state.Account.Username)
	if err != nil {
		return err
	}
	for _, m := range history {
		seen[m.ID] = true
	}
	for {
		lock, err := s.lock(ctx)
		if err != nil {
			return err
		}
		state, err = s.state()
		if err == nil && state.Account == nil {
			err = errors.New("native account logged out")
		}
		var batch native.SyncBatch
		if err == nil {
			batch, err = connection.Sync(ctx, state.Account, state.Cursor)
		}
		var messages []native.Message
		if err == nil {
			for _, cmd := range batch.Commands {
				if cmd.ID != 5 {
					continue
				}
				m, decodeErr := native.DecodeMessage(cmd.Payload, state.Account.Username)
				if decodeErr != nil {
					fmt.Fprintln(os.Stderr, "Retained an undecoded native message in the private sync journal.")
					continue
				}
				if !seen[m.ID] {
					messages = append(messages, m)
					seen[m.ID] = true
				}
			}
			// Retain all raw commands before committing the cursor. A crash can
			// replay a batch, but cannot discard its unknown message types.
			if len(batch.Commands) > 0 {
				err = s.journal(nativeJournal{Owner: state.Account.Username, Batch: batch, Messages: messages})
			}
			if err == nil {
				state.Cursor = batch.Cursor
				err = s.save("state.json", state)
			}
		}
		lock.Close()
		if err != nil {
			return err
		}
		for _, m := range messages {
			if *asJSON {
				err = json.NewEncoder(os.Stdout).Encode(m)
			} else {
				_, err = fmt.Fprintf(os.Stdout, "%s %s: %s\n", time.Unix(int64(m.Timestamp), 0).Format(time.RFC3339), m.From, m.Text)
			}
			if err != nil {
				return err
			}
		}
		if batch.Continue {
			continue
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func nativeSend(ctx context.Context, s nativeStore, args []string) error {
	f := flags("send")
	to := f.String("to", "", "native username/wxid learned from read")
	text := f.String("text", "", "text to send")
	stdin := f.Bool("stdin", false, "read message text from standard input")
	replyID := f.String("reply-to", "", "quote this archived server message ID")
	if err := parse(f, args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("--to is required")
	}
	textSet := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "text" {
			textSet = true
		}
	})
	if *stdin {
		if textSet {
			return errors.New("use either --text or --stdin")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 60*1024+1))
		if err != nil {
			return err
		}
		*text = string(data)
	}
	if strings.TrimSpace(*text) == "" || len(*text) > 60*1024 {
		return errors.New("message must contain 1–61440 bytes")
	}
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Account == nil {
		return errors.New("not logged in natively; run wechat login")
	}
	service := &chatService{store: s, owner: state.Account.Username}
	var result chatSendResult
	if *replyID != "" {
		snapshot, err := s.chatSnapshot(state.Account.Username)
		if err != nil {
			return err
		}
		var target *native.Message
		for i := range snapshot.Messages {
			m := &snapshot.Messages[i]
			if m.ID == *replyID && m.Peer == *to {
				target = m
				break
			}
		}
		if target == nil {
			return errors.New("reply target is not in this conversation's archive")
		}
		result = service.reply(ctx, *target, *text, publicQuoteName(snapshot.Contacts, *target))
	} else {
		result = service.send(ctx, *to, *text)
	}
	if result.Err != nil {
		return result.Err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"id": result.Message.ID, "status": "accepted"})
}
