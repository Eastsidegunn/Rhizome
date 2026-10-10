package main

// RHZ-107 G1 (FR-RHZ-141, SCP §6): `rhizome secret …` is a thin HTTP client
// of a running serve. It never opens the journal or takes its lock, has no
// value input path, and prints only fixed strings plus server fields that
// pass the §0.1 grammar: no flag value, argument, address or response body
// is ever echoed.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
)

// Fixed CLI messages (§6). Nothing is ever appended except a §0.2 Reason.
const (
	secretUsage       = "secret: usage"
	secretRejected    = "secret: rejected"
	secretUnreachable = "secret: serve unreachable"
	secretServeError  = "secret: serve error"
	secretUnavailable = "secret: unavailable"

	secretDefaultAddr  = "127.0.0.1:8080" // serve's -addr default
	secretDefaultActor = "rhizome-cli"
	secretMaxBody      = 16 << 20
)

// secretKnownReasons is the §0.2 list; only these are printed after
// "secret: rejected: ".
var secretKnownReasons = map[string]bool{
	secretref.ReasonChannelOnly:       true,
	secretref.ReasonMalformed:         true,
	secretref.ReasonInvalidOpID:       true,
	secretref.ReasonInvalidName:       true,
	secretref.ReasonInvalidRefKind:    true,
	secretref.ReasonInvalidScopeTags:  true,
	secretref.ReasonInvalidRef:        true,
	secretref.ReasonInvalidReasonCode: true,
	secretref.ReasonInvalidActor:      true,
	secretref.ReasonOpIDConflict:      true,
	secretref.ReasonUnknownRef:        true,
	secretref.ReasonRefNotActive:      true,
	secretref.ReasonRefBusy:           true,
	secretref.ReasonNameInUse:         true,
	secretref.ReasonUnavailable:       true,
	secretref.ReasonExternallyManaged: true,
}

// errSecretUsage marks a usage failure (exit 2); every other failure is a
// fixed message with exit 1.
var errSecretUsage = errors.New(secretUsage)

type secretFail struct{ msg string }

func (e secretFail) Error() string { return e.msg }

// secretNewOpID is G1: crypto/rand 16 bytes. A variable so tests can fail it.
var secretNewOpID = func() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "op-" + hex.EncodeToString(b), nil
}

// secretTags collects repeated -scope-tag values; a duplicate is a usage error.
type secretTags []string

func (t *secretTags) String() string { return "" }
func (t *secretTags) Set(v string) error {
	for _, x := range *t {
		if x == v {
			return errSecretUsage
		}
	}
	*t = append(*t, v)
	return nil
}

func secretCmd(args []string, out, errOut io.Writer) int {
	var (
		body []byte
		err  error
	)
	if len(args) == 0 {
		fmt.Fprintln(errOut, secretUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub == "list" {
		body, err = secretList(rest)
	} else {
		body, err = secretRequest(sub, rest)
	}
	if err != nil {
		if errors.Is(err, errSecretUsage) {
			fmt.Fprintln(errOut, secretUsage)
			return 2
		}
		var f secretFail
		if errors.As(err, &f) {
			fmt.Fprintln(errOut, f.msg)
		} else {
			fmt.Fprintln(errOut, secretServeError)
		}
		return 1
	}
	out.Write(body)
	return 0
}

// secretParse parses f over args; any flag error (including -h) or a
// leftover positional argument is a usage error.
func secretParse(f *flag.FlagSet, args []string) error {
	f.SetOutput(io.Discard)
	if f.Parse(args) != nil || f.NArg() != 0 {
		return errSecretUsage
	}
	return nil
}

// secretSet reports which flags were given explicitly.
func secretSet(f *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	f.Visit(func(v *flag.Flag) { set[v.Name] = true })
	return set
}

func secretClient() *http.Client {
	// Never route loopback traffic to a proxy from the environment, and
	// never follow a redirect: a 3xx is a non-200 answer (serve error) and a
	// POST is never replayed elsewhere.
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// secretDo performs one request; a transport failure is "serve unreachable"
// and the body is read only for status 200.
func secretDo(req *http.Request, err error) (int, []byte, error) {
	if err != nil {
		return 0, nil, secretFail{secretUnreachable}
	}
	res, err := secretClient().Do(req)
	if err != nil {
		return 0, nil, secretFail{secretUnreachable}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, secretMaxBody+1))
	if err != nil || len(b) > secretMaxBody {
		return 0, nil, secretFail{secretServeError}
	}
	return res.StatusCode, b, nil
}

// --- list ---

type cliSecretOp struct {
	OpID string `json:"opId"`
	Op   string `json:"op"`
}

type cliSecretOutcome struct {
	OpID        string `json:"opId"`
	Op          string `json:"op"`
	Result      string `json:"result"`
	FailureCode string `json:"failureCode,omitempty"`
}

type cliSecretObserved struct {
	Present         bool   `json:"present"`
	Version         int64  `json:"version"`
	PlaneObservedAt string `json:"planeObservedAt"`
}

// cliSecret mirrors the §4.2 DTO field for field (same order and
// omitempty), so -json re-encodes exactly the keys secretsSpec admitted.
type cliSecret struct {
	Ref          string             `json:"ref"`
	Name         string             `json:"name"`
	Kind         string             `json:"kind"`
	Origin       string             `json:"origin"`
	State        string             `json:"state"`
	Version      int64              `json:"version"`
	ScopeTags    []string           `json:"scopeTags"`
	Pending      *cliSecretOp       `json:"pending,omitempty"`
	LastOutcome  *cliSecretOutcome  `json:"lastOutcome,omitempty"`
	RegisteredAt string             `json:"registeredAt,omitempty"`
	RotatedAt    string             `json:"rotatedAt,omitempty"`
	RevokedAt    string             `json:"revokedAt,omitempty"`
	Observed     *cliSecretObserved `json:"observed,omitempty"`
}

func validSecretState(s string) bool {
	switch s {
	case secretref.StatePendingRegistration, secretref.StateRegistrationFailed, secretref.StateActive,
		secretref.StateRotating, secretref.StateRevoking, secretref.StateRevoked:
		return true
	}
	return false
}

func validSecretOrigin(s string) bool {
	return s == secretref.OriginRequest || secretwire.ValidOrigin(s)
}

// validDTOScopeTags is G4 as the DTO renders it: an empty list is [].
func validDTOScopeTags(tags []string) bool { return len(tags) == 0 || secretwire.ValidScopeTags(tags) }

// secretsSpec is the exact GET /v1/secrets envelope (§4.2, location B):
// exact keys, no null, no duplicates, every field in its §0.1 grammar, and
// failureCode present iff result is "failed" (G8). Anything else is a
// serve error; nothing of it is printed.
var secretsSpec = func() *secretwire.Spec {
	op := secretwire.Object(map[string]secretwire.Field{
		"opId": secretwire.Required(secretwire.String(secretwire.ValidOpID)),
		"op":   secretwire.Required(secretwire.String(secretwire.ValidOp)),
	})
	outcome := func(result string, failed bool) *secretwire.Spec {
		f := map[string]secretwire.Field{
			"opId":   secretwire.Required(secretwire.String(secretwire.ValidOpID)),
			"op":     secretwire.Required(secretwire.String(secretwire.ValidOp)),
			"result": secretwire.Required(secretwire.Enum(result)),
		}
		if failed {
			f["failureCode"] = secretwire.Required(secretwire.String(secretwire.ValidFailureCode))
		}
		return secretwire.Object(f)
	}
	version := secretwire.Int(0, secretwire.MaxVersion)
	at := secretwire.String(secretwire.ValidObservedAt)
	dto := secretwire.Object(map[string]secretwire.Field{
		"ref":       secretwire.Required(secretwire.String(secretwire.ValidRef)),
		"name":      secretwire.Required(secretwire.String(secretwire.ValidName)),
		"kind":      secretwire.Required(secretwire.String(secretwire.ValidRefKind)),
		"origin":    secretwire.Required(secretwire.String(validSecretOrigin)),
		"state":     secretwire.Required(secretwire.String(validSecretState)),
		"version":   secretwire.Required(version),
		"scopeTags": secretwire.Required(secretwire.StringList(validDTOScopeTags)),
		"pending":   secretwire.Optional(op),
		"lastOutcome": secretwire.Optional(secretwire.Union("result", map[string]*secretwire.Spec{
			secretwire.ResultOK:       outcome(secretwire.ResultOK, false),
			secretwire.ResultRejected: outcome(secretwire.ResultRejected, false),
			secretwire.ResultFailed:   outcome(secretwire.ResultFailed, true),
		})),
		"registeredAt": secretwire.Optional(at),
		"rotatedAt":    secretwire.Optional(at),
		"revokedAt":    secretwire.Optional(at),
		"observed": secretwire.Optional(secretwire.Object(map[string]secretwire.Field{
			"present":         secretwire.Required(secretwire.Bool()),
			"version":         secretwire.Required(version),
			"planeObservedAt": secretwire.Required(at),
		})),
	})
	return secretwire.Object(map[string]secretwire.Field{
		"revision": secretwire.Required(secretwire.Int(0, secretwire.MaxSafeInt)),
		"body": secretwire.Required(secretwire.Object(map[string]secretwire.Field{
			"secrets": secretwire.Required(secretwire.Array(dto, 0, -1)),
		})),
	})
}()

// intentAnswerSpec is the exact /v1/intent answer for a secret kind
// (RelayResult: Accepted, Reason, and opId/ref only when accepted).
var intentAnswerSpec = secretwire.Object(map[string]secretwire.Field{
	"Accepted": secretwire.Required(secretwire.Bool()),
	"Reason":   secretwire.Required(secretwire.String(func(string) bool { return true })),
	"opId":     secretwire.Optional(secretwire.String(secretwire.ValidOpID)),
	"ref":      secretwire.Optional(secretwire.String(secretwire.ValidRef)),
})

func secretCell(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func secretList(args []string) ([]byte, error) {
	f := flag.NewFlagSet("secret list", flag.ContinueOnError)
	addr := f.String("addr", secretDefaultAddr, "")
	asJSON := f.Bool("json", false, "")
	if err := secretParse(f, args); err != nil {
		return nil, err
	}
	code, b, err := secretDo(http.NewRequest(http.MethodGet, "http://"+*addr+"/v1/secrets", nil))
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, secretFail{secretUnavailable}
	}
	if code != http.StatusOK {
		return nil, secretFail{secretServeError}
	}
	var env struct {
		Body struct {
			Secrets []cliSecret `json:"secrets"`
		} `json:"body"`
	}
	if secretwire.StrictUnmarshal(b, secretsSpec, &env) != nil {
		return nil, secretFail{secretServeError}
	}
	rows := env.Body.Secrets // non-nil: the spec requires the array
	var buf bytes.Buffer
	if *asJSON {
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(rows) != nil {
			return nil, secretFail{secretServeError}
		}
		return buf.Bytes(), nil
	}
	buf.WriteString("ref\tname\tkind\tstate\tversion\torigin\tpending\n")
	for _, r := range rows {
		pending := ""
		if r.Pending != nil {
			pending = r.Pending.Op
		}
		buf.WriteString(strings.Join([]string{secretCell(r.Ref), secretCell(r.Name), secretCell(r.Kind),
			secretCell(r.State), strconv.FormatInt(r.Version, 10), secretCell(r.Origin), secretCell(pending)}, "\t"))
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// --- request intents ---

// secretIntentBody has exactly the §2.1 keys; scopeTags is omitted when empty.
type secretIntentBody struct {
	Kind       string   `json:"kind"`
	Actor      string   `json:"actor"`
	OpID       string   `json:"opId"`
	Name       string   `json:"name,omitempty"`
	RefKind    string   `json:"refKind,omitempty"`
	ScopeTags  []string `json:"scopeTags,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	ReasonCode string   `json:"reasonCode,omitempty"`
}

func secretRequest(sub string, args []string) ([]byte, error) {
	f := flag.NewFlagSet("secret "+sub, flag.ContinueOnError)
	addr := f.String("addr", secretDefaultAddr, "")
	opID := f.String("op-id", "", "")
	actor := f.String("actor", secretDefaultActor, "")
	var name, kind, ref, reason *string
	var tags secretTags
	var required []string
	in := secretIntentBody{}
	switch sub {
	case "register-request":
		in.Kind = secretref.KindRegisterRequest
		name, kind = f.String("name", "", ""), f.String("kind", "", "")
		f.Var(&tags, "scope-tag", "")
		required = []string{"name", "kind"}
	case "rotate-request":
		in.Kind = secretref.KindRotateRequest
		ref = f.String("ref", "", "")
		required = []string{"ref"}
	case "revoke-request":
		in.Kind = secretref.KindRevokeRequest
		ref, reason = f.String("ref", "", ""), f.String("reason", "", "")
		required = []string{"ref", "reason"}
	default:
		return nil, errSecretUsage
	}
	if err := secretParse(f, args); err != nil {
		return nil, err
	}
	set := secretSet(f)
	for _, r := range required {
		if !set[r] {
			return nil, errSecretUsage
		}
	}
	in.Actor, in.OpID = *actor, *opID
	if !set["op-id"] {
		id, err := secretNewOpID()
		if err != nil {
			return nil, secretFail{secretUnavailable}
		}
		in.OpID = id
	}
	switch {
	case name != nil:
		in.Name, in.RefKind = *name, *kind
		in.ScopeTags = append([]string(nil), tags...)
		sort.Strings(in.ScopeTags)
	case reason != nil:
		in.Ref, in.ReasonCode = *ref, *reason
	default:
		in.Ref = *ref
	}
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	enc.SetEscapeHTML(false)
	if enc.Encode(in) != nil {
		return nil, errSecretUsage
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+*addr+"/v1/intent", &payload)
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	code, b, err := secretDo(req, err)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, secretFail{secretServeError}
	}
	var res struct {
		Accepted bool
		Reason   string
		OpID     string `json:"opId"`
		Ref      string `json:"ref"`
	}
	if secretwire.StrictUnmarshal(b, intentAnswerSpec, &res) != nil {
		return nil, secretFail{secretServeError}
	}
	if !res.Accepted {
		if res.OpID != "" || res.Ref != "" {
			return nil, secretFail{secretServeError}
		}
		if secretKnownReasons[res.Reason] {
			return nil, secretFail{secretRejected + ": " + res.Reason}
		}
		return nil, secretFail{secretRejected}
	}
	// An accepted answer must name the op we sent and its derived ref.
	if res.Reason != "" || res.OpID != in.OpID || res.Ref != secretwire.DeriveRef(res.OpID) {
		return nil, secretFail{secretServeError}
	}
	return []byte(res.OpID + "\t" + res.Ref + "\n"), nil
}
