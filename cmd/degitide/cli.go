package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/notify"
	"github.com/tatoalo/degitide/internal/scheduler"
	"golang.org/x/term"
)

const cliHelp = `degitide — your portfolio, on your server

  degitide serve                 Run the server and scheduled syncs (default)
  degitide setup                 Create the administrator from your terminal
  degitide accounts              List broker accounts
  degitide accounts add --name ISA --id isa
  degitide connect --account isa Connect a broker account (hidden prompts)
  degitide sync                  Sync all accounts, showing progress
  degitide sync --account isa    Sync just one account
  degitide status --json         Read status without waking the browser worker
  degitide open --ssh user@vps   Open the dashboard through an SSH tunnel
  degitide sure token --base-url http://degitide-sure:8391
                                Create a one-time Sure SimpleFIN setup token
  degitide sure status           List connections and their last read
  degitide sure revoke --id ID   Revoke one connection immediately
  degitide version               Print the installed version

Run commands inside Docker with: docker compose exec app degitide <command>
The CLI uses the mounted dashboard token. Run setup once before other commands.
Use --url for another local or HTTPS endpoint; credentials stay out of URLs.
Use connect/setup --credentials-stdin to read one JSON object from stdin.
Use open --no-open to print the URL and keep the tunnel in the foreground.
`

type cliClient struct {
	base, token string
	http        *http.Client
}

func runCLI(args []string) (bool, error) {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println("degitide " + version)
		return true, nil
	}
	if len(args) == 0 || (len(args) == 1 && args[0] == "serve") {
		return false, nil
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(cliHelp)
		return true, nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "notify-serve" {
		return true, notify.Serve(ctx)
	}
	if args[0] == "open" {
		return true, openDashboard(ctx, args[1:])
	}
	command := args[0]
	rest := args[1:]
	sureAction := "status"
	if command == "sure" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sureAction, rest = rest[0], rest[1:]
	}
	add := command == "accounts" && len(rest) > 0 && rest[0] == "add"
	if add {
		rest = rest[1:]
	}
	if command != "setup" && command != "accounts" && command != "connect" && command != "sync" && command != "status" && command != "sure" {
		return true, fmt.Errorf("unknown command %q; run degitide help", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	endpoint := flags.String("url", envOr("DEGITIDE_URL", "http://127.0.0.1:8391"), "dashboard URL")
	account := flags.String("account", "", "broker account ID; sync defaults to all")
	name := flags.String("name", "", "new account name")
	id := flags.String("id", "", "new account ID")
	sureBaseURL := flags.String("base-url", "", "Degitide origin reachable from Sure")
	sureActivity := flags.Bool("include-transactions", false, "include gross trade activity as ordinary Sure ledger entries, not native trades")
	asJSON := flags.Bool("json", false, "machine-readable output")
	stdin := flags.Bool("credentials-stdin", false, "read credential JSON from stdin")
	codeOnly := flags.Bool("code-only", false, "connect using a fresh code without saving automatic-login credentials")
	timeout := flags.Duration("timeout", 30*time.Minute, "maximum sync wait")
	if err := flags.Parse(rest); err != nil {
		return true, err
	}
	if flags.NArg() != 0 {
		return true, errors.New("unexpected arguments; flags follow the command")
	}
	if *account != "" && command != "sure" && !db.ValidAccountID(*account) {
		return true, errors.New("invalid account ID")
	}
	if err := validateDashboardURL(*endpoint); err != nil {
		return true, err
	}
	token := os.Getenv("DEGITIDE_API_TOKEN")
	if path := os.Getenv("DEGITIDE_API_TOKEN_FILE"); path != "" {
		value, err := os.ReadFile(path)
		if err != nil {
			return true, errors.New("cannot read dashboard token file")
		}
		token = strings.TrimSpace(string(value))
	}
	jar, _ := cookiejar.New(nil)
	client := &cliClient{base: strings.TrimSuffix(*endpoint, "/"), token: token, http: &http.Client{Jar: jar, Timeout: 230 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	query := ""
	if *account != "" {
		query = "?account=" + url.QueryEscape(*account)
	}
	switch command {
	case "sure":
		switch sureAction {
		case "token":
			if *sureBaseURL == "" {
				return true, errors.New("--base-url is required: use the Degitide URL reachable from Sure")
			}
			ids := []string{}
			if *account != "" {
				ids = strings.Split(*account, ",")
			}
			var result struct {
				ID        string    `json:"id"`
				Token     string    `json:"setup_token"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if err := client.api(ctx, "POST", "/api/integrations/sure", map[string]any{"base_url": *sureBaseURL, "name": *name, "account_ids": ids, "include_transactions": *sureActivity}, &result); err != nil {
				return true, err
			}
			if *asJSON {
				return true, printJSON(result)
			}
			fmt.Fprintf(os.Stderr, "Paste this token into Sure → Add account → SimpleFIN. Expires %s. Connection: %s\n", result.ExpiresAt.Format(time.RFC3339), result.ID)
			fmt.Println(result.Token)
			return true, nil
		case "status":
			var result any
			if err := client.api(ctx, "GET", "/api/integrations/sure", nil, &result); err != nil {
				return true, err
			}
			return true, printJSON(result)
		case "revoke":
			if *id == "" {
				return true, errors.New("--id is required")
			}
			if err := client.api(ctx, "DELETE", "/api/integrations/sure/"+url.PathEscape(*id), map[string]any{}, nil); err != nil {
				return true, err
			}
			fmt.Println("Sure connection revoked.")
			return true, nil
		default:
			return true, errors.New("use sure token, sure status, or sure revoke --id ID")
		}
	case "setup":
		credentials, err := readCredentials(*stdin, false, false)
		if err != nil {
			return true, err
		}
		return true, client.setup(ctx, credentials)
	case "connect":
		credentials, err := readCredentials(*stdin, true, *codeOnly)
		if err != nil {
			return true, err
		}
		var result any
		if err = client.api(ctx, "POST", "/api/degiro/reauthenticate"+query, credentials, &result); err != nil {
			return true, err
		}
		fmt.Println("Account connected. Run degitide sync to import its holdings.")
		return true, nil
	case "accounts":
		var result any
		method, body := "GET", any(nil)
		if add {
			method = "POST"
			body = map[string]string{"id": *id, "name": *name}
		}
		if err := client.api(ctx, method, "/api/accounts", body, &result); err != nil {
			return true, err
		}
		return true, printJSON(result)
	case "status":
		var status struct {
			Account  db.Account         `json:"account"`
			Progress scheduler.Progress `json:"progress"`
			Running  bool               `json:"pull_running"`
			Error    string             `json:"pull_error"`
		}
		if err := client.api(ctx, "GET", "/api/status"+query, nil, &status); err != nil {
			return true, err
		}
		if *asJSON {
			return true, printJSON(status)
		}
		fmt.Printf("%s (%s)\nLast sync: %s\n", status.Account.Name, status.Account.ID, orNever(status.Account.LastPull))
		if status.Running {
			fmt.Printf("Syncing: %d%% · %s\n", status.Progress.Percent, status.Progress.Stage)
		}
		if status.Account.LastError != "" {
			fmt.Printf("Needs attention: %s\n", status.Account.LastError)
		}
		return true, nil
	case "sync":
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		if err := client.api(ctx, "POST", "/api/pull/trigger"+query, map[string]any{}, nil); err != nil {
			return true, err
		}
		previous := ""
		for {
			var status struct {
				Progress scheduler.Progress `json:"progress"`
				Running  bool               `json:"pull_running"`
				Error    string             `json:"pull_error"`
			}
			if err := client.api(ctx, "GET", "/api/status"+query, nil, &status); err != nil {
				return true, err
			}
			line := fmt.Sprintf("%3d%%  %s", status.Progress.Percent, status.Progress.Stage)
			if line != previous {
				if *asJSON {
					_ = printJSON(status.Progress)
				} else {
					fmt.Println(line)
				}
				previous = line
			}
			if !status.Running {
				if status.Error != "" {
					return true, errors.New(status.Error)
				}
				return true, nil
			}
			select {
			case <-ctx.Done():
				return true, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return true, nil
}

func (c *cliClient) api(ctx context.Context, method, path string, body, out any) error {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(encoded)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, input)
	if err != nil {
		return err
	}
	r.Header.Set("Origin", c.base)
	r.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		r.SetBasicAuth("degitide", c.token)
	}
	response, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("cannot reach Degitide: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		var result struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result)
		if result.Error == "" {
			result.Error = response.Status
		}
		return errors.New(result.Error)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(out)
	}
	return nil
}

var csrfPattern = regexp.MustCompile(`name="csrf_token"[^>]*value="([A-Za-z0-9_-]+)"`)

func (c *cliClient) setup(ctx context.Context, credentials map[string]string) error {
	r, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/setup", nil)
	response, err := c.http.Do(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != 200 {
		return errors.New("setup is unavailable or an administrator already exists")
	}
	match := csrfPattern.FindSubmatch(body)
	if len(match) != 2 {
		return errors.New("could not obtain setup form token")
	}
	form := url.Values{"username": {credentials["username"]}, "password": {credentials["password"]}, "password_confirm": {credentials["password"]}, "csrf_token": {string(match[1])}}
	r, _ = http.NewRequestWithContext(ctx, "POST", c.base+"/setup", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", c.base)
	response, err = c.http.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		return errors.New("setup failed; use a valid username and a password of 12–256 characters")
	}
	fmt.Println("Administrator created. Add broker accounts with degitide accounts add, then run degitide connect.")
	return nil
}

func readCredentials(fromStdin, broker, codeOnly bool) (map[string]string, error) {
	if fromStdin {
		var fields struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Seed     string `json:"seed"`
			TOTP     string `json:"totp"`
		}
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4097))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&fields); err != nil {
			return nil, errors.New("expected credential JSON on stdin")
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return nil, errors.New("expected one credential JSON object")
		}
		return map[string]string{"username": fields.Username, "password": fields.Password, "seed": fields.Seed, "totp": fields.TOTP}, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errors.New("interactive terminal required; use --credentials-stdin for automation")
	}
	reader := bufio.NewReader(os.Stdin)
	label := "Degitide"
	if broker {
		label = "DEGIRO"
	}
	fmt.Fprint(os.Stderr, label+" username: ")
	username, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fmt.Fprint(os.Stderr, label+" password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	result := map[string]string{"username": strings.TrimSpace(username), "password": string(password)}
	clear(password)
	if broker {
		key, prompt := "seed", "Authenticator setup secret: "
		if codeOnly {
			key, prompt = "totp", "Fresh six-digit code: "
		}
		fmt.Fprint(os.Stderr, prompt)
		secret, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		result[key] = string(secret)
		clear(secret)
	}
	return result, nil
}

func validateDashboardURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("use a dashboard origin without credentials, path or query")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackBind(u.Hostname())) {
		return errors.New("remote dashboards require HTTPS; use an SSH tunnel for loopback HTTP")
	}
	return nil
}

var sshDestination = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\[\]-]*$`)

func sshArgs(destination string, localPort, remotePort int) ([]string, error) {
	if !sshDestination.MatchString(destination) || localPort < 1 || localPort > 65535 || remotePort < 1 || remotePort > 65535 {
		return nil, errors.New("invalid SSH destination or port")
	}
	return []string{"-N", "-T", "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort), "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3", "--", destination}, nil
}

func openDashboard(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("open", flag.ContinueOnError)
	destination := flags.String("ssh", "", "SSH destination or host alias")
	localPort := flags.Int("local-port", 18391, "local tunnel port")
	remotePort := flags.Int("port", 8391, "remote loopback dashboard port")
	noOpen := flags.Bool("no-open", false, "print the URL without opening a browser")
	endpoint := flags.String("url", envOr("DEGITIDE_URL", "http://127.0.0.1:8391"), "local or HTTPS dashboard URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected open arguments")
	}
	if *destination == "" {
		if err := validateDashboardURL(*endpoint); err != nil {
			return err
		}
		fmt.Println(*endpoint)
		if !*noOpen {
			return launchBrowser(*endpoint)
		}
		return nil
	}
	arguments, err := sshArgs(*destination, *localPort, *remotePort)
	if err != nil {
		return err
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(*localPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("local port %d is already in use", *localPort)
	}
	listener.Close()
	command := exec.CommandContext(ctx, "ssh", arguments...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	opened := false
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("SSH tunnel closed: %w", err)
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			if !opened {
				return errors.New("SSH tunnel did not become ready within two minutes")
			}
		case <-ticker.C:
			if opened {
				continue
			}
			conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
			if err != nil {
				continue
			}
			conn.Close()
			opened = true
			*endpoint = "http://" + address
			fmt.Printf("Dashboard: %s\nKeep this terminal open. Ctrl+C closes the tunnel; server syncs keep running.\n", *endpoint)
			if !*noOpen {
				if err := launchBrowser(*endpoint); err != nil {
					fmt.Fprintln(os.Stderr, "Open the URL above in your browser.")
				}
			}
		}
	}
}

func launchBrowser(value string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", value)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", value)
	default:
		command = exec.Command("xdg-open", value)
	}
	return command.Run()
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func orNever(value string) string {
	if value == "" {
		return "never"
	}
	return value
}
