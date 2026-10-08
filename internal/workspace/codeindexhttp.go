package workspace

// RHZ-059 (FR-RHZ-089): GET /v1/codeindex — 조회 시점 생성. serve가 자기
// writer 경계(h.Store, lock을 쥔 단일 writer 프로세스)로 처음 본 main sha에
// main.advanced를 emit한다. 감시·폴링 없음. 그래프는 이벤트로 적재하지 않는다.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"rhizome/internal/codeindex"
	"rhizome/internal/events"
)

func writeCacheAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".graph-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (h *HTTPServer) serveCodeIndex(w http.ResponseWriter, r *http.Request) {
	if h.IndexRepo == "" || h.IndexOut == "" {
		// 미배선 = 비활성 (ExecEvents·Blobs nil 관례).
		http.NotFound(w, r)
		return
	}
	if storePoisoned(h.Store) {
		servePoisoned(w)
		return
	}
	sha, err := codeindex.ResolveMain(h.IndexRepo)
	if err != nil {
		http.Error(w, "code index unavailable", http.StatusInternalServerError)
		return
	}
	cachePath := filepath.Join(h.IndexOut, sha, "graph.json")
	b, err := os.ReadFile(cachePath)
	if err != nil {
		// Build is fail-stop: on error nothing is written and nothing emitted
		// (serve-side partial artifacts stay at zero).
		g, err := codeindex.Build(h.IndexRepo, sha)
		if err != nil {
			http.Error(w, "code index build failed", http.StatusInternalServerError)
			return
		}
		b = codeindex.Marshal(g)
		// Atomic write (tmp + rename): a concurrent reader on the HIT path
		// must only ever see whole-old or whole-new bytes, never a torn
		// O_TRUNC write. Bytes are deterministic, so concurrent renames of
		// the same sha are harmless.
		if err := writeCacheAtomic(cachePath, b); err != nil {
			http.Error(w, "code index cache write failed", http.StatusInternalServerError)
			return
		}
	}
	// [최신 읽기 → append]는 임계구역: 같은 fresh sha에 몰린 동시 조회가
	// 중복 emit하지 않게 하는 1차 보증. revision 가드는 2차 백스톱.
	h.indexMu.Lock()
	prevSha, prevRev, err := codeindex.LatestMainAdvanced(h.Store.List("repo", "main"))
	if err == nil && prevSha != sha {
		raw, me := json.Marshal(codeindex.MainAdvancedPayload{Sha: sha, At: time.Now().UTC().Format(time.RFC3339)})
		if me != nil {
			err = me
		} else {
			err = h.Store.Append(prevRev, events.Event{AggregateType: "repo", AggregateID: "main", Revision: prevRev + 1, Type: "main.advanced", Payload: raw})
		}
	}
	h.indexMu.Unlock()
	if err != nil {
		if storePoisoned(h.Store) || errors.Is(err, events.ErrPoisoned) {
			servePoisoned(w)
			return
		}
		http.Error(w, "code index record failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}
