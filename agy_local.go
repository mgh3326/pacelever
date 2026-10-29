package pacelever

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Antigravity app's local quota source. Ported from ClaudeBar (MIT),
// Modules/DataSources/Sources/Internal/Fetch/LocalServerFetcher.swift and the
// antigravity `local` dataSource in
// Modules/Providers/Resources/Providers/antigravity.json: find the running
// language server, read --csrf_token and --extension_server_port from its
// command line, and POST Connect-RPC calls to the loopback port. No OAuth
// token, keychain read or credential write-back is involved, so it is strictly
// safer than the cloud source and is tried first. The csrf token is only ever
// sent to 127.0.0.1 URLs constructed here — never to a host read from the
// process or anywhere else.
var (
	agyLocalNames = []string{"language_server", "language_server_macos", "language_server_macos_arm", "agy"}
	agyLocalMatch = []*regexp.Regexp{
		regexp.MustCompile(`(^|/|\s)agy(\s|$)`),
		regexp.MustCompile(`--app_data_dir.*antigravity`),
		regexp.MustCompile(`antigravity.*--app_data_dir`),
		regexp.MustCompile(`/antigravity/`),
		regexp.MustCompile(`\.antigravity/`),
	}
	agyLocalCSRF = regexp.MustCompile(`--csrf_token[=\s]+([^\s]+)`)
	agyLocalPort = regexp.MustCompile(`--extension_server_port[=\s]+([^\s]+)`)
)

var agyLocalPaths = []string{
	"/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary",
	"/exa.language_server_pb.LanguageServerService/GetUserStatus",
	"/exa.language_server_pb.LanguageServerService/GetCommandModelConfigs",
}

const agyLocalRequestBody = `{"metadata":{"ideName":"antigravity","extensionName":"antigravity","ideVersion":"unknown","locale":"en"}}`

type agyLocalProc struct {
	pid  int
	argv string
}

// agyLocalMatch reports whether a command line is the Antigravity app's
// server: it names one of the known binaries and carries an antigravity
// marker, so an unrelated language_server or an agy-named CLI is skipped.
func agyLocalIsServer(argv string) bool {
	lower := strings.ToLower(strings.ReplaceAll(argv, "\\", "/"))
	named := false
	for _, name := range agyLocalNames {
		if strings.Contains(lower, name) {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	for _, pattern := range agyLocalMatch {
		if pattern.MatchString(lower) {
			return true
		}
	}
	return false
}

func agyLocalValue(pattern *regexp.Regexp, argv string) string {
	match := pattern.FindStringSubmatch(argv)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func agyLocalPortNumber(argv string) int {
	port, e := strconv.Atoi(agyLocalValue(agyLocalPort, argv))
	if e != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// agyLocalProcesses lists candidate servers via `pgrep -a -lf`, the same
// listing ClaudeBar reads. A missing pgrep or an empty table is not an error:
// the local source simply does not answer.
func agyLocalProcesses(ctx context.Context) []agyLocalProc {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "pgrep", "-a", "-lf", strings.Join(agyLocalNames, "|")).Output()
	if e != nil {
		return nil
	}
	var procs []agyLocalProc
	for _, line := range strings.Split(string(out), "\n") {
		pid, rest, found := strings.Cut(strings.TrimSpace(line), " ")
		n, e := strconv.Atoi(pid)
		if !found || e != nil {
			continue
		}
		procs = append(procs, agyLocalProc{pid: n, argv: strings.TrimSpace(rest)})
	}
	return procs
}

// agyListenPorts reads the process's listening TCP ports via lsof, the
// fallback endpoints ClaudeBar tries over TLS when the plain extension port
// does not answer.
func agyListenPorts(ctx context.Context, pid int) []int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", strconv.Itoa(pid)).Output()
	if e != nil {
		return nil
	}
	portRe := regexp.MustCompile(`:(\d+)\s+\(LISTEN\)`)
	seen := map[int]bool{}
	var ports []int
	for _, match := range portRe.FindAllStringSubmatch(string(out), -1) {
		if port, e := strconv.Atoi(match[1]); e == nil && port > 0 && port <= 65535 && !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	return ports
}

// agyLoopbackClient is used only for the 127.0.0.1 URLs this file builds. The
// app serves a self-signed certificate on its TLS port; certificate checks are
// skipped for these loopback calls only. Redirects are not followed.
func agyLoopbackClient() HTTPDoer {
	return &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

// local asks the running app for quota. It returns ok=false on any failure —
// no process, missing csrf token, every endpoint refused, an unparseable
// answer — so the caller always falls back to the cloud path.
func (p agyProvider) local(ctx context.Context) (Observation, bool) {
	if p.localProcs == nil {
		return Observation{}, false
	}
	procs := p.localProcs(ctx)
	sort.Slice(procs, func(i, j int) bool { return procs[i].pid < procs[j].pid })
	for _, proc := range procs {
		if !agyLocalIsServer(proc.argv) {
			continue
		}
		csrf := agyLocalValue(agyLocalCSRF, proc.argv)
		if csrf == "" {
			continue
		}
		client := p.localHTTP
		if client == nil {
			client = agyLoopbackClient()
		}
		type endpoint struct {
			scheme string
			port   int
		}
		var tries []endpoint
		if p.localPorts != nil {
			for _, port := range p.localPorts(ctx, proc.pid) {
				tries = append(tries, endpoint{"https", port})
			}
		}
		if port := agyLocalPortNumber(proc.argv); port > 0 {
			tries = append(tries, endpoint{"http", port})
		}
		headers := map[string]string{
			"Content-Type":             "application/json",
			"X-Codeium-Csrf-Token":     csrf,
			"Connect-Protocol-Version": "1",
		}
		for _, try := range tries {
			for _, path := range agyLocalPaths {
				resp, e := doJSON(ctx, client, "POST", fmt.Sprintf("%s://127.0.0.1:%d%s", try.scheme, try.port, path), headers, []byte(agyLocalRequestBody))
				if e != nil || resp.Status != 200 || resp.ParseError {
					continue
				}
				if o := parseAgy(agyLocalShape(resp.Body), nil); o.Error == "" {
					return o, true
				}
				// The app answered 200 with an unrecognized shape; that is a
				// local failure, not a reason to ask it again.
				return Observation{}, false
			}
		}
	}
	return Observation{}, false
}

// agyLocalShape normalizes the two per-model Connect-RPC answers
// (GetUserStatus's userStatus.clientModelConfigs and GetCommandModelConfigs's
// clientModelConfigs) into the models map parseAgy already reads, carrying the
// plan name through the same paidTier field the cloud summary uses.
func agyLocalShape(body map[string]any) map[string]any {
	root := body
	if response := object(body["response"]); len(response) > 0 {
		root = response
	}
	if len(list(root["groups"])) > 0 || len(object(root["models"])) > 0 {
		return body
	}
	status := object(root["userStatus"])
	configs := list(object(status["cascadeModelConfigData"])["clientModelConfigs"])
	if len(configs) == 0 {
		configs = list(root["clientModelConfigs"])
	}
	if len(configs) == 0 {
		return body
	}
	models := map[string]any{}
	for _, value := range configs {
		entry := object(value)
		label := str(entry["label"])
		if label == "" {
			continue
		}
		models[label] = map[string]any{"displayName": label, "quotaInfo": entry["quotaInfo"]}
	}
	out := map[string]any{"models": models}
	if name := str(object(object(status["planStatus"])["planInfo"])["planName"]); name != "" {
		out["plan"] = map[string]any{"paidTier": map[string]any{"name": name}}
	}
	return out
}
