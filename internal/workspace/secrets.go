package workspace

// RHZ-107 G1 (FR-RHZ-135, SCP §2, §4): the three secret request intents and
// the read-only GET /v1/secrets view. No surface here carries a secret value
// and no refusal echoes input: every Reason is a fixed §0.2 string.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"rhizome/internal/secretref"
	"rhizome/internal/secretwire"
)

const secretKindPrefix = "secret."

// relaySecret applies I0 and hands the three request kinds to the single
// secret writer (I2–I7). A nil writer refuses with X15 after the format
// checks (secretref.Service methods are nil-safe). The error is non-nil
// only for events.ErrPoisoned (§1.8 MJ-1a → 503).
func relaySecret(svc *secretref.Service, in Intent, actor string) (RelayResult, error) {
	var (
		res secretref.IntentResult
		err error
	)
	switch in.Kind {
	case secretref.KindRegisterRequest:
		res, err = svc.RequestRegister(secretref.RegisterRequest{OpID: in.OpID, Name: in.Name,
			RefKind: in.RefKind, ScopeTags: in.ScopeTags, Actor: actor})
	case secretref.KindRotateRequest:
		res, err = svc.RequestRotate(secretref.RotateRequest{OpID: in.OpID, Ref: in.Ref, Actor: actor})
	case secretref.KindRevokeRequest:
		res, err = svc.RequestRevoke(secretref.RevokeRequest{OpID: in.OpID, Ref: in.Ref,
			ReasonCode: in.ReasonCode, Actor: actor})
	default: // I0: fact kinds (and anything else under "secret.") are channel-only
		return RelayResult{Reason: secretref.ReasonChannelOnly}, nil
	}
	if err != nil {
		return RelayResult{}, err
	}
	if !res.Accepted {
		return RelayResult{Reason: res.Reason}, nil
	}
	return RelayResult{Accepted: true, OpID: res.OpID, Ref: res.Ref}, nil
}

// secretIntentSpec is the I1 shape (SCP §2.1 key sets, V-K exact case; V-J
// grammar per §0.1). The kind selects the variant; any other kind under
// "secret." never reaches I1 (I0 comes first).
var secretIntentSpec = func() *secretwire.Spec {
	common := func(kind string, fields map[string]secretwire.Field) *secretwire.Spec {
		fields["kind"] = secretwire.Required(secretwire.Enum(kind))
		fields["actor"] = secretwire.Required(secretwire.String(secretwire.ValidActor))
		fields["opId"] = secretwire.Required(secretwire.String(secretwire.ValidOpID))
		return secretwire.Object(fields)
	}
	return secretwire.Union("kind", map[string]*secretwire.Spec{
		secretref.KindRegisterRequest: common(secretref.KindRegisterRequest, map[string]secretwire.Field{
			"name":      secretwire.Required(secretwire.String(secretwire.ValidName)),
			"refKind":   secretwire.Required(secretwire.String(secretwire.ValidRefKind)),
			"scopeTags": secretwire.Optional(secretwire.StringList(secretwire.ValidScopeTags)),
		}),
		secretref.KindRotateRequest: common(secretref.KindRotateRequest, map[string]secretwire.Field{
			"ref": secretwire.Required(secretwire.String(secretwire.ValidRef)),
		}),
		secretref.KindRevokeRequest: common(secretref.KindRevokeRequest, map[string]secretwire.Field{
			"ref":        secretwire.Required(secretwire.String(secretwire.ValidRef)),
			"reasonCode": secretwire.Required(secretwire.String(secretwire.ValidReasonCode)),
		}),
	})
}()

// secretIntentWire is the strictly decoded request; its keys were matched
// exactly by secretIntentSpec before encoding/json sees them.
type secretIntentWire struct {
	Kind       string   `json:"kind"`
	Actor      string   `json:"actor"`
	OpID       string   `json:"opId"`
	Name       string   `json:"name"`
	RefKind    string   `json:"refKind"`
	ScopeTags  []string `json:"scopeTags"`
	Ref        string   `json:"ref"`
	ReasonCode string   `json:"reasonCode"`
}

// decodeSecretIntent is I1: V-J 0–6 and the §2.1 key set over the whole raw
// request body. ok=false is X2; the reason never says which key or rule.
func decodeSecretIntent(body []byte) (in Intent, actor string, ok bool) {
	var w secretIntentWire
	if secretwire.StrictUnmarshal(body, secretIntentSpec, &w) != nil {
		return Intent{}, "", false
	}
	return Intent{Kind: w.Kind, OpID: w.OpID, Name: w.Name, RefKind: w.RefKind,
		ScopeTags: w.ScopeTags, Ref: w.Ref, ReasonCode: w.ReasonCode}, w.Actor, true
}

// secretKindProbe reads only "kind" from the first JSON value of the body
// with the lenient decoder. It decides which path the body takes and is
// never stored; "" means not a secret kind.
func secretKindProbe(raw json.RawMessage) string {
	var probe struct{ Kind string }
	if json.Unmarshal(raw, &probe) != nil || !strings.HasPrefix(probe.Kind, secretKindPrefix) {
		return ""
	}
	return probe.Kind
}

// --- GET /v1/secrets (SCP §4.2, location B) ---

type secretsEnvelope struct {
	Revision uint64      `json:"revision"`
	Body     secretsBody `json:"body"`
}

type secretsBody struct {
	Secrets []secretDTO `json:"secrets"`
}

type secretDTO struct {
	Ref          string             `json:"ref"`
	Name         string             `json:"name"`
	Kind         string             `json:"kind"`
	Origin       string             `json:"origin"`
	State        string             `json:"state"`
	Version      int64              `json:"version"`
	ScopeTags    []string           `json:"scopeTags"`
	Pending      *secretOpDTO       `json:"pending,omitempty"`
	LastOutcome  *secretOutcomeDTO  `json:"lastOutcome,omitempty"`
	RegisteredAt string             `json:"registeredAt,omitempty"`
	RotatedAt    string             `json:"rotatedAt,omitempty"`
	RevokedAt    string             `json:"revokedAt,omitempty"`
	Observed     *secretObservedDTO `json:"observed,omitempty"`
}

type secretOpDTO struct {
	OpID string `json:"opId"`
	Op   string `json:"op"`
}

type secretOutcomeDTO struct {
	OpID        string `json:"opId"`
	Op          string `json:"op"`
	Result      string `json:"result"`
	FailureCode string `json:"failureCode,omitempty"`
}

type secretObservedDTO struct {
	Present         bool   `json:"present"`
	Version         int64  `json:"version"`
	PlaneObservedAt string `json:"planeObservedAt"`
}

// dtoTime is an envelope CreatedAt as UTC RFC3339 seconds; zero = omitted.
func dtoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func toSecretDTO(v secretref.RefView) secretDTO {
	d := secretDTO{Ref: v.Ref, Name: v.Name, Kind: v.Kind, Origin: v.Origin, State: v.State,
		Version: v.Version, ScopeTags: append([]string{}, v.ScopeTags...),
		RegisteredAt: dtoTime(v.RegisteredAt), RotatedAt: dtoTime(v.RotatedAt), RevokedAt: dtoTime(v.RevokedAt)}
	if v.Pending != nil {
		d.Pending = &secretOpDTO{OpID: v.Pending.OpID, Op: v.Pending.Op}
	}
	if o := v.LastOutcome; o != nil {
		d.LastOutcome = &secretOutcomeDTO{OpID: o.OpID, Op: o.Op, Result: o.Result, FailureCode: o.FailureCode}
	}
	if o := v.Observed; o != nil {
		d.Observed = &secretObservedDTO{Present: o.Present, Version: o.Version, PlaneObservedAt: o.PlaneObservedAt}
	}
	return d
}

// serveSecrets is GET /v1/secrets: a pure read (works after poison), never
// part of /v1/context. Revision is the journal's largest Sequence, the same
// definition as the /v1/workspace envelope, so a reader can pair the two.
func (h *HTTPServer) serveSecrets(w http.ResponseWriter, r *http.Request) {
	if h.Secrets == nil {
		http.NotFound(w, r)
		return
	}
	p, rev, err := h.Secrets.Snapshot()
	if err != nil {
		var re *secretref.ReplayError
		if errors.As(err, &re) {
			http.Error(w, re.Error(), http.StatusInternalServerError)
			return
		}
		http.Error(w, secretref.ReasonUnavailable, http.StatusInternalServerError)
		return
	}
	out := secretsEnvelope{Revision: rev, Body: secretsBody{Secrets: []secretDTO{}}}
	for _, v := range p.Refs() {
		out.Body.Secrets = append(out.Body.Secrets, toSecretDTO(v))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
