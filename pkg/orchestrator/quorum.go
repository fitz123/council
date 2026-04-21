package orchestrator

// Quorum logic is inlined in Run because it is a three-line check that
// does not benefit from its own function (premature abstraction would
// obscure the control flow). This file exists as a documentation
// surface: the rule is codified here so readers looking for "where is
// the quorum rule" have a landing spot.
//
// Rule (design/v1.md §10, Quorum subsection):
//
//	survivors = count(experts where status == "ok")
//	if survivors < profile.Quorum:
//	    write verdict with status="quorum_failed" and return ErrQuorumFailed
//
// Note strict less-than: `survivors == quorum` passes. A quorum of 1
// with 1 survivor is a valid run; a quorum of 2 with 1 survivor fails.
// MVP default (see defaults/default.yaml Task 8) is quorum=1, which
// means any single-expert success is enough to keep the pipeline going.
