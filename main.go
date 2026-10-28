package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kevmo314/wechat-cli/internal/native"
)

const usage = `wechat — standalone Go WeChat CLI

Usage: wechat [--config-dir DIR] [--backend native|ilink] [--profile phone|tablet|desktop] [COMMAND] [OPTIONS]

  (no command)                           Open the interactive WeChat client
  tui      [--offline]                   Open the TUI / browse local history

  login    [--qr FILE.png] [--timeout 8m]   Pair using a WeChat QR code
  translate [--id ID | --text TEXT] [--lang en]  Translate using WeChat
  mark-read --to ID                       Sync conversation read state to other devices
  attachment --id ID [--output FILE] [--preview]  Download / preview an attachment
  wait     [--timeout 5m] [--json] [--from ID]  Wait for a new incoming message
  read     [--follow] [--json]             Synchronize available messages
  send     --to USER [--text TEXT | --stdin]
  contacts                               List users seen by read
  history  [--limit N]                    Browse the native local archive
  status                                 Show local account details
  logout                                 Remove local credentials and cursor
  version                                Print CLI version

Global options must precede the command. Use COMMAND --help for options.
The default native backend uses desktop QR login and native sync (experimental).
Use --backend ilink for the original ClawBot API and existing bot credentials.
Native history contains messages synchronized by this client, not all past chats.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) || errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintln(os.Stderr, "wechat:", err)
		os.Exit(1)
	}
}
func flags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	return nil
}
func run(ctx context.Context, args []string) error {
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	root := flags("wechat")
	dir := root.String("config-dir", filepath.Join(config, "wechat-cli"), "directory for private account state")
	backend := root.String("backend", "native", "native desktop protocol or ilink bot API")
	profile := root.String("profile", "", "phone, tablet, or desktop; each uses separate saved credentials")
	root.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := root.Parse(args); err != nil {
		return err
	}
	command, commandArgs := "tui", root.Args()
	if root.NArg() > 0 {
		command, commandArgs = root.Arg(0), root.Args()[1:]
	}
	args = commandArgs
	if command == "help" {
		fmt.Print(usage)
		return nil
	}
	if command == "version" {
		fmt.Println("wechat-cli 0.3.0 (native desktop experimental; iLink compatibility " + protocolVersion + ")")
		return nil
	}
	store := Store{*dir}
	explicitDir := false
	root.Visit(func(f *flag.Flag) {
		if f.Name == "config-dir" {
			explicitDir = true
		}
	})
	if *backend == "native" {
		requested := *profile
		if requested != "" {
			canonical, err := native.NormalizeProfile(requested)
			if err != nil {
				return err
			}
			requested = canonical
		}
		loginProfilePresent := false
		if command == "login" {
			local, present, err := loginProfileArgument(args)
			if err != nil {
				return err
			}
			loginProfilePresent = present
			if requested != "" && present && local != requested {
				return errors.New("global and login --profile options disagree")
			}
			if requested == "" {
				requested = local
			}
		}
		if command == "tui" {
			for {
				chosen := store
				if requested != "" {
					chosen, err = profileStore(store, config, requested, explicitDir)
					if err != nil {
						return err
					}
				} else {
					chosen, err = preferredProfileStore(store, config, explicitDir)
					if err != nil {
						return err
					}
				}
				if requested != "" {
					if err = rememberProfile(store, requested); err != nil {
						return err
					}
				}
				err = nativeTUI(ctx, chosen, args, requested)
				var change *profileSwitchRequest
				if errors.As(err, &change) {
					requested = change.Profile
					continue
				}
				return err
			}
		}
		if requested == "" {
			store, err = preferredProfileStore(store, config, explicitDir)
			if err != nil {
				return err
			}
		}
		if requested != "" {
			store, err = profileStore(store, config, requested, explicitDir)
			if err != nil {
				return err
			}
			if command == "login" && *profile != "" && !loginProfilePresent {
				args = append(args, "--profile", requested)
			}
		}
		return nativeCommand(ctx, store, command, args)
	}
	if *profile != "" {
		return errors.New("--profile requires the native backend")
	}
	if *backend != "ilink" {
		return errors.New("backend must be native or ilink")
	}
	switch command {
	case "login":
		return login(ctx, store, args)
	case "read":
		return readMessages(ctx, store, args)
	case "send":
		return sendMessage(ctx, store, args)
	case "status", "contacts", "logout":
		if err := parse(flags(command), args); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q; use wechat --help", command)
	}
	if command == "logout" {
		lock, err := store.readerLock()
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := store.update(func(s *State) error { *s = State{}; return nil }); err != nil {
			return err
		}
		fmt.Println("Local credentials removed. This does not revoke the binding in WeChat.")
		return nil
	}
	state, err := store.load()
	if err != nil {
		return err
	}
	if command == "status" {
		if state.Token == "" {
			fmt.Println("Not logged in")
			return nil
		}
		fmt.Printf("Bot: %s\nUser: %s\nAPI: %s\nKnown conversations: %d\n", state.BotID, state.UserID, state.BaseURL, len(state.Contexts))
		return nil
	}
	users := make([]string, 0, len(state.Contexts))
	for user := range state.Contexts {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		fmt.Println(user)
	}
	return nil
}
func account(store Store) (State, *Client, error) {
	s, err := store.load()
	if err != nil {
		return s, nil, err
	}
	if s.Token == "" {
		return s, nil, errors.New("not logged in; run wechat login")
	}
	base, err := trustedBase(s.BaseURL)
	if err != nil {
		return s, nil, err
	}
	return s, newClient(base, s.Token), nil
}

func login(ctx context.Context, store Store, args []string) error {
	f := flags("login")
	png := f.String("qr", "", "also save the QR code as a PNG file")
	timeout := f.Duration("timeout", 8*time.Minute, "overall login timeout")
	if err := parse(f, args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	lock, err := store.readerLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	old, err := store.load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client := newClient(defaultBaseURL, "")
	tokens := []string{}
	if old.Token != "" {
		tokens = append(tokens, old.Token)
	}
	display := func(q QRCode) error {
		return displayLoginQR(ctx, client.HTTP, os.Stderr, q, *png)
	}
	input := bufio.NewReader(os.Stdin)
	verify := func(ctx context.Context) (string, error) {
		fmt.Fprint(os.Stderr, "Enter the verification code shown in WeChat: ")
		type result struct {
			text string
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := input.ReadString('\n')
			ch <- result{strings.TrimSpace(line), err}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-ch:
			if r.err != nil {
				return "", fmt.Errorf("read verification code: %w", r.err)
			}
			if r.text == "" {
				return "", errors.New("verification code is empty")
			}
			return r.text, nil
		}
	}
	status, err := performLogin(ctx, client, tokens, display, verify)
	if err != nil {
		return err
	}
	if status.Status == "binded_redirect" {
		if old.Token == "" {
			return errors.New("bot already bound but no local credentials exist; rebind it in WeChat")
		}
		fmt.Println("Already connected; keeping existing credentials.")
		return nil
	}
	if err := store.update(func(s *State) error {
		*s = State{Token: status.Token, BotID: status.BotID, UserID: status.UserID, BaseURL: status.BaseURL, Contexts: map[string]string{}}
		return nil
	}); err != nil {
		return err
	}
	fmt.Println("Logged in as", status.BotID)
	return nil
}

func performLogin(ctx context.Context, client *Client, tokens []string, display func(QRCode) error, verify func(context.Context) (string, error)) (LoginStatus, error) {
	initialBase := client.BaseURL
	var qr QRCode
	var code string
	attempts := 0
	refresh := func() error {
		attempts++
		if attempts > 3 {
			return errors.New("QR login expired or verification blocked three times; try login again")
		}
		client.BaseURL = initialBase
		code = ""
		var err error
		qr, err = client.qr(ctx, tokens)
		if err != nil {
			return err
		}
		return display(qr)
	}
	if err := refresh(); err != nil {
		return LoginStatus{}, err
	}
	for {
		status, err := client.loginStatus(ctx, qr.Code, code)
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if err != nil {
			if !isTimeout(err) {
				return status, err
			}
		} else {
			switch status.Status {
			case "confirmed":
				if status.Token == "" || status.BotID == "" {
					return status, errors.New("confirmed login missing credentials")
				}
				if status.BaseURL == "" {
					status.BaseURL = client.BaseURL
				}
				status.BaseURL, err = trustedBase(status.BaseURL)
				return status, err
			case "binded_redirect":
				return status, nil
			case "wait":
			case "scaned":
				code = ""
			case "need_verifycode":
				code, err = verify(ctx)
				if err != nil {
					return status, err
				}
				continue
			case "expired", "verify_code_blocked":
				if err := refresh(); err != nil {
					return status, err
				}
				continue
			case "scaned_but_redirect":
				base, err := trustedBase("https://" + status.RedirectHost)
				if err != nil {
					return status, err
				}
				client.BaseURL = base
			default:
				return status, fmt.Errorf("unrecognized login status %q", status.Status)
			}
		}
		if err := pause(ctx, time.Second); err != nil {
			return LoginStatus{}, err
		}
	}
}

func readMessages(ctx context.Context, store Store, args []string) error {
	f := flags("read")
	follow := f.Bool("follow", false, "continuously long-poll until Ctrl-C")
	raw := f.Bool("json", false, "emit original messages as newline-delimited JSON (includes context tokens)")
	if err := parse(f, args); err != nil {
		return err
	}
	lock, err := store.readerLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	state, client, err := account(store)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		updates, err := client.updates(ctx, state.Cursor)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) || (!*follow && !isTimeout(err)) {
				return err
			}
			if !*follow {
				return nil
			}
			fmt.Fprintln(os.Stderr, "Poll failed; retrying:", err)
			if err := pause(ctx, backoff); err != nil {
				return err
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		contexts := map[string]string{}
		for _, data := range updates.Messages {
			var msg Message
			if err := json.Unmarshal(data, &msg); err != nil {
				return errors.New("invalid incoming message; cursor not advanced")
			}
			if msg.From != "" && msg.Context != "" && msg.Type == 1 {
				contexts[msg.From] = msg.Context
			}
			if *raw {
				err = json.NewEncoder(os.Stdout).Encode(json.RawMessage(data))
			} else {
				_, err = fmt.Fprintf(os.Stdout, "%s\t%s\n", terminalText(msg.From), terminalText(msg.text()))
			}
			if err != nil {
				return err
			}
		}
		// Commit only after output succeeds. A crash can replay a batch, but won't silently skip it.
		if err := store.update(func(s *State) error {
			if s.Token != state.Token {
				return errors.New("account changed while reading")
			}
			for user, token := range contexts {
				s.Contexts[user] = token
			}
			if updates.Cursor != "" {
				s.Cursor = updates.Cursor
			}
			state.Cursor = s.Cursor
			return nil
		}); err != nil {
			return err
		}
		if updates.TimeoutMS > 0 {
			client.HTTP.Timeout = time.Duration(min(updates.TimeoutMS, 300000))*time.Millisecond + 5*time.Second
		}
		if !*follow {
			return nil
		}
		if err := pause(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}
func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' || r == 127 {
			return -1
		}
		return r
	}, text)
}
func sendMessage(ctx context.Context, store Store, args []string) error {
	f := flags("send")
	to := f.String("to", "", "recipient ID from read/contacts")
	text := f.String("text", "", "message text")
	stdin := f.Bool("stdin", false, "read message text from standard input")
	token := f.String("context-token", "", "override cached conversation token")
	if err := parse(f, args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("--to is required")
	}
	if *stdin {
		if *text != "" {
			return errors.New("use either --text or --stdin")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return errors.New("message exceeds 1 MiB")
		}
		*text = string(data)
	}
	if strings.TrimSpace(*text) == "" {
		return errors.New("provide a nonempty --text or --stdin message")
	}
	state, client, err := account(store)
	if err != nil {
		return err
	}
	if *token == "" {
		*token = state.Contexts[*to]
	}
	if *token == "" {
		return errors.New("no conversation token for recipient; receive their message with wechat read first, or supply --context-token")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, err := client.send(ctx, *to, *text, *token)
	if err != nil {
		return fmt.Errorf("send failed (not automatically retried; delivery may be uncertain): %w", err)
	}
	fmt.Println(id)
	return nil
}
