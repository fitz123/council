// Package debate implements the v2 multi-round anonymized debate + voting
// pipeline. Experts run blind in round 1, see each other's outputs (behind
// single-letter labels) in round 2, then vote on the final answer.
//
// This file contains the anonymization layer: a deterministic session-ID
// -> (label -> real_name) mapping, so every round and every resume derive
// the same labels without persisting the map explicitly.
package debate

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
)

// Expert is a participant in the debate. Only Name is needed for
// anonymization; richer fields (Executor, Model, Timeout, PromptBody) are
// carried on config.RoleConfig and consulted by the round runners that own
// subprocess execution.
type Expert struct {
	Name string
}

// maxLabels is the v2 label-alphabet cap. v3 extends to A1, A2, ... once a
// real deployment demonstrates the need.
const maxLabels = 26

// AssignLabels returns a map from anonymized label (A, B, C, ...) to real
// expert name. The mapping is deterministic for a given session ID: the
// SHA-256 of the session ID seeds a PCG RNG that shuffles the expert order.
// Resume only needs the session ID to re-derive the same labels, so the
// mapping never has to be persisted as a separate artifact.
//
// Returns an error if len(experts) > 26 (v2's label-alphabet cap).
func AssignLabels(sessionID string, experts []Expert) (map[string]string, error) {
	if len(experts) > maxLabels {
		return nil, fmt.Errorf("AssignLabels: %d experts exceeds the %d-label cap (v2 supports A..Z only; v3 will extend to A1, A2, ...)", len(experts), maxLabels)
	}
	digest := sha256.Sum256([]byte(sessionID))
	seedHi := binary.BigEndian.Uint64(digest[0:8])
	seedLo := binary.BigEndian.Uint64(digest[8:16])
	rng := rand.New(rand.NewPCG(seedHi, seedLo))

	shuffled := make([]Expert, len(experts))
	copy(shuffled, experts)
	rng.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	mapping := make(map[string]string, len(shuffled))
	for i, e := range shuffled {
		label := string(rune('A' + i))
		mapping[label] = e.Name
	}
	return mapping, nil
}

// LabelOf returns the anonymized label for a real expert name, or "", false
// if the name is not present in the mapping. Orchestrator code uses this to
// address an expert by label when it only has the real name in hand (e.g.,
// when iterating over config.Profile.Experts).
func LabelOf(real string, mapping map[string]string) (string, bool) {
	for label, name := range mapping {
		if name == real {
			return label, true
		}
	}
	return "", false
}
