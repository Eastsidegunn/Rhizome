// Package archtest pins the ratified kernel boundary in CI (RHZ-055,
// FR-RHZ-085): substrate ← {exec kernel ∥ knowledge kernel} ← bridge ←
// assembly ← surface. The two kernels never import each other (R1), and the
// allowed-exception list is the single source of truth for violations —
// exact match both ways (R5). Policy lives here, not in prose.
package archtest

import (
	"fmt"
	"sort"
)

// Layers classifies every internal package (R4: unclassified = FAIL). A new
// package must pick its layer to get past CI.
var Layers = map[string]string{
	"events": "substrate", "journal": "substrate", "blob": "substrate", "policy": "substrate",
	"peercred": "substrate", "termio": "substrate",
	// codeindex reads git objects and the events port only (RHZ-058/059) —
	// substrate keeps that minimality enforced via R2.
	"codeindex": "substrate",
	"domain":    "exec", "mission": "exec", "projector": "exec", "decision": "exec", "coordinator": "exec",
	"wake": "exec", "execution": "exec", "approval": "exec", "gaterequest": "exec", "question": "exec",
	"janusadapter": "exec", "audit": "exec",
	"memory": "knowledge", "source": "knowledge", "knowledge": "knowledge", "relation": "knowledge",
	"procedure": "knowledge", "retrieval": "knowledge", "evaluation": "knowledge",
	// edge moved from the handoff's exec to bridge: since RHZ-057 an edge
	// links memory (knowledge) to goal/mission (exec) and its exists() check
	// reads both kernels — it is cross-kernel linking vocabulary, which is
	// what bridge means. The alternative (keep exec + an edge->memory
	// exception) would record the same fact as a lingering violation.
	"trace": "bridge", "deliverable": "bridge", "edge": "bridge",
	// assembly instantiates templates across both kernels (RHZ-063). R6: it
	// may import substrate/exec/knowledge/bridge, never surface. R7: within
	// internal/, only surface may import assembly (cmd, outside internal/,
	// is the other legal consumer and outside this scan).
	"assembly":  "assembly",
	"workspace": "surface", "surface": "surface",
	"archtest": "test",
}

// Allowed is the exact exception list (R5): a violation not listed fails,
// and a listed entry that is no longer a violation fails too.
var Allowed = []string{
	"coordinator->memory", // 완료 결정의 근거 memory 확인 (ApplyDecision)
	"coordinator->source", // 완료 산출물 원문 등록 (ensureDeliverable, RHZ-048 D28)
}

// violates says whether a package in fromLayer may import one in toLayer.
func violates(fromLayer, toLayer string) bool {
	switch fromLayer {
	case "substrate":
		return toLayer != "substrate" // R2
	case "exec":
		return toLayer == "knowledge" || toLayer == "surface" || toLayer == "assembly" // R1, R3, R7
	case "knowledge":
		return toLayer == "exec" || toLayer == "surface" || toLayer == "assembly" // R1, R3, R7
	case "bridge":
		return toLayer == "surface" || toLayer == "assembly" // R3, R7
	case "assembly":
		return toLayer == "surface" // R6
	case "surface", "test":
		return false
	}
	return true
}

// Check evaluates the rules over imports (package → imported internal
// packages, bare names) and returns the violations, sorted. Pure: no
// filesystem, so each rule is unit-pinnable with crafted inputs.
func Check(imports map[string][]string, layers map[string]string, allowed []string) []string {
	violations := map[string]bool{}
	pkgs := make([]string, 0, len(imports))
	for pkg := range imports {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		fromLayer, ok := layers[pkg]
		if !ok {
			violations[fmt.Sprintf("unclassified:%s", pkg)] = true // R4
			continue
		}
		for _, imp := range imports[pkg] {
			if imp == pkg {
				continue
			}
			toLayer, ok := layers[imp]
			if !ok {
				violations[fmt.Sprintf("unclassified:%s", imp)] = true // R4
				continue
			}
			if violates(fromLayer, toLayer) {
				violations[pkg+"->"+imp] = true
			}
		}
	}
	// R5: exceptions match exactly — absorb listed violations, flag stale ones.
	out := []string{}
	for _, exception := range allowed {
		if violations[exception] {
			delete(violations, exception)
		} else {
			out = append(out, "stale exception: "+exception)
		}
	}
	for v := range violations {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
