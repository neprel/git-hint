package project

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/report"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

var errActivityPrivacy = errors.New("activity privacy projection refused")

// errLedgerProfileUnsupported is deliberately static: exact nested instance timestamps
// cannot satisfy a truncating profile. Lossy/coarse ledger support remains deferred.
var errLedgerProfileUnsupported = errors.New("model_ledger_profile_unsupported")

// activityPrivacyInputs limits dependencies to emitted rows. Only emitted membership
// units retain their full predecessor frontier; controlled-only exports need no journal.
func activityPrivacyInputs(all, emitted []activity.Observation) ([]activity.Observation, error) {
	frontier := map[string]int{}
	wanted := map[string]bool{}
	epoch := func(o activity.Observation) (int, error) {
		m, err := snapshot.Plain(o.Membership)
		if err != nil {
			return 0, errActivityPrivacy
		}
		events, ok := m["events"].([]any)
		if !ok || len(events) == 0 {
			return 0, errActivityPrivacy
		}
		first, ok := events[0].(map[string]any)
		if !ok {
			return 0, errActivityPrivacy
		}
		n, ok := first["epoch"].(float64)
		if !ok || n < 1 || n > 1000 || n != float64(int(n)) {
			return 0, errActivityPrivacy
		}
		return int(n), nil
	}
	for _, o := range emitted {
		wanted[o.ObservationID] = true
		if o.Membership == nil {
			continue
		}
		n, err := epoch(o)
		if err != nil {
			return nil, err
		}
		if n > frontier[o.Membership.UnitRef] {
			frontier[o.Membership.UnitRef] = n
		}
	}
	out := make([]activity.Observation, 0, len(emitted))
	for _, o := range all {
		if wanted[o.ObservationID] {
			out = append(out, o)
			continue
		}
		if o.Membership == nil || frontier[o.Membership.UnitRef] == 0 {
			continue
		}
		n, err := epoch(o)
		if err != nil {
			return nil, err
		}
		if n <= frontier[o.Membership.UnitRef] {
			out = append(out, o)
		}
	}
	return out, nil
}

// activityPrivacy owns copied wire values. Only BuildPackage constructs it from received
// local state; no caller supplies a translation dictionary, callback, or authority flag.
type activityPrivacy struct {
	observations map[string]map[string]any
	sources      map[string]activity.Source // indexed by LOCAL source ID
}

type privacyEpoch struct {
	observation activity.Observation
	body        map[string]any
	number      int
}

func newActivityPrivacy(s *store.Store, observations []activity.Observation, sources map[string]activity.Source, now time.Time) (*activityPrivacy, error) {
	key := s.Key()
	if len(key) != 32 {
		return nil, errActivityPrivacy
	}
	p := &activityPrivacy{observations: map[string]map[string]any{}, sources: map[string]activity.Source{}}
	epochs := map[string][]privacyEpoch{}
	seen := map[string]bool{}
	for _, o := range observations {
		if o.ModelLedger == nil && o.Membership == nil {
			continue
		}
		if seen[o.ObservationID] {
			return nil, errActivityPrivacy
		}
		seen[o.ObservationID] = true
		src, ok := sources[o.SourceID]
		if !ok || o.ModelLedger != nil && o.Membership != nil {
			return nil, errActivityPrivacy
		}
		// Reuse the normal closed LOCAL batch/schema/semantic validator, before projection.
		b := activityBatch{Version: 1, Source: src, Observations: []activity.Observation{o}}
		var e error
		b.Digest, e = activityBatchDigest(b)
		if e != nil || validateActivityBatch(s, b) != nil {
			return nil, errActivityPrivacy
		}
		var expected activity.Observation
		if m := o.ModelLedger; m != nil {
			if src.Configuration == nil || m.Binding.ConfigurationDigest != src.Configuration.Digest() || m.LedgerRef != activity.LedgerRef(key, m.Binding.RunRef) {
				return nil, errActivityPrivacy
			}
			expectedSource := activity.LedgerSource(key, src.Agent, *src.Configuration)
			if !reflect.DeepEqual(src, expectedSource) {
				return nil, errActivityPrivacy
			}
			expected = activity.LedgerObservation(key, src, *m)
			// The source version is explicit, independent of the unchanged private source/spool.
			wireSource := expectedSource
			wireSource.Adapter.Version = version.ActivityPrivacyVersion
			wireSource.SourceID = hint.DeriveID(key, "source", "hint-model-ledger", version.ActivityPrivacyVersion, src.Configuration.Digest())
			p.sources[o.SourceID] = wireSource
			wire := privacyObservation(o)
			wire["source_id"] = wireSource.SourceID
			wire["model_ledger"] = privacyLedger(key, *m)
			p.observations[o.ObservationID] = wire
		} else {
			m := o.Membership
			if len(m.Events) == 0 {
				return nil, errActivityPrivacy
			}
			plain, e := snapshot.Plain(m)
			if e != nil {
				return nil, errActivityPrivacy
			}
			events, _ := plain["events"].([]any)
			first, _ := events[0].(map[string]any)
			n, ok := first["epoch"].(float64)
			if !ok || n < 1 || n > 1000 || n != float64(int(n)) {
				return nil, errActivityPrivacy
			}
			batch := producerBatch(key, m.UnitRef, int(n), *m, o.Provenance.CapturedAt)
			if !reflect.DeepEqual(src, batch.Source) {
				return nil, errActivityPrivacy
			}
			expected = batch.Observations[0]
			wireSource := batch.Source
			wireSource.Adapter.Version = "0.2.0"
			p.sources[o.SourceID] = wireSource // current orchestrator source derivation excludes version
			epochs[m.UnitRef] = append(epochs[m.UnitRef], privacyEpoch{o, plain, int(n)})
		}
		if !equalJSON(o, expected) {
			return nil, errActivityPrivacy
		}
	}
	if len(epochs) > 0 {
		if e := p.projectEpochs(s, epochs, now); e != nil {
			return nil, e
		}
	}
	// Copy every pointer/map before exposing a wire value to later filtering/coarsening.
	for id, m := range p.observations {
		cp, e := snapshot.Plain(m)
		if e != nil {
			return nil, errActivityPrivacy
		}
		p.observations[id] = cp
	}
	for id, src := range p.sources {
		cp, e := snapshot.Plain(src)
		if e != nil {
			return nil, errActivityPrivacy
		}
		var copied activity.Source
		if e = decodePrivacySource(cp, &copied); e != nil {
			return nil, e
		}
		p.sources[id] = copied
	}
	return p, nil
}

func privacyObservation(o activity.Observation) map[string]any {
	m := map[string]any{"observation_id": o.ObservationID, "class": o.Class, "op": o.Op, "source": o.Source, "source_id": o.SourceID,
		"precision": o.Precision, "time": o.Time, "outcome": o.Outcome, "missing": o.Missing, "provenance": o.Provenance}
	if len(o.Correlation) > 0 {
		m["correlation"] = o.Correlation
	}
	if o.Usage != nil {
		m["usage"] = o.Usage
	}
	return m
}

func privacyLedger(key []byte, l activity.ModelLedger) map[string]any {
	counts, coverage, components := map[string]any{}, map[string]any{}, map[string]any{}
	for _, k := range activity.LedgerCounts {
		counts[k] = l.Counts[k]
	}
	for _, k := range activity.LedgerComponents {
		coverage[k] = l.ComponentCoverage[k]
		components[k] = l.Components[k]
	}
	b := l.Binding
	binding := map[string]any{"run_ref": b.RunRef, "task_id": b.TaskID, "task_family_id": b.TaskFamilyID, "configuration_digest": b.ConfigurationDigest, "unit_ref": b.UnitRef, "attempt_ref": b.AttemptRef}
	if b.Origin != "" {
		binding["origin"] = b.Origin
		binding["execution_ref"] = b.ExecutionRef
	}
	m := map[string]any{"ledger_ref": l.LedgerRef, "revision": l.Revision, "state": l.State, "reasons": l.Reasons, "counts": counts,
		"components": components, "component_coverage": coverage, "model_check": l.ModelCheck, "bypass": l.Bypass, "scope": l.Scope, "sealed_at": l.SealedAt, "binding": binding}
	if len(l.Entries) > 0 {
		es := []any{}
		for _, e := range l.Entries {
			c := map[string]any{}
			for _, k := range activity.LedgerComponents {
				c[k] = e.Components[k]
			}
			es = append(es, map[string]any{"ordinal": e.Ordinal, "entry_id": e.EntryID, "state": e.State, "termination": e.Termination, "response_started": e.ResponseStarted, "stream_done": e.StreamDone, "status_class": e.StatusClass, "model_check": e.ModelCheck, "admitted_ms": e.AdmittedMs, "finished_ms": e.FinishedMs, "components": c})
		}
		m["entries"], m["anchor_utc"], m["sealed_ms"], m["finalized_ms"], m["clock_step_ms"] = es, l.AnchorUTC, l.SealedMs, l.FinalizedMs, l.ClockStepMs
	}
	if a := l.AgentRun; a != nil {
		m["agent_run"] = map[string]any{"outcome": a.Outcome, "exit_code": a.ExitCode, "enclosure_start_ms": a.EnclosureStartMs, "enclosure_end_ms": a.EnclosureEndMs,
			"execution_provenance_ref": hint.DeriveID(key, "execution-provenance", a.ExecutionDigest), "engine_started_at": a.EngineStartedAt, "engine_finished_at": a.EngineFinishedAt}
	}
	if c := l.BackendCapture; c != nil {
		m["backend_capture"] = map[string]any{"method": c.Method, "collector": c.Collector, "start": privacyCapture(key, c.Start), "end": privacyCapture(key, c.End)}
	}
	return m
}

func privacyCapture(key []byte, c activity.LedgerCapture) map[string]any {
	m := map[string]any{"status": c.Status, "send_ms": c.SendMs, "recv_ms": c.RecvMs, "snapshot": nil}
	if s := c.Snapshot; s != nil {
		m["snapshot"] = map[string]any{"image_ref": hint.DeriveID(key, "image", s.ImageID), "host_ref": s.HostRef, "container_ref": s.ContainerRef, "started_at": s.StartedAt, "restart_count": s.RestartCount, "flags": s.Flags}
	}
	return m
}

func (p *activityPrivacy) projectEpochs(s *store.Store, groups map[string][]privacyEpoch, now time.Time) error {
	// Controlled-only exports never load project journals. WorkUnits supplies actual labels;
	// LoadWork rereads the exact selected unit, then the existing body validator/checker owns shape.
	units, e := WorkUnits(s)
	if e != nil {
		return errActivityPrivacy
	}
	received := map[string]*WorkUnit{}
	for _, u := range units {
		if _, needed := groups[u.UnitRef]; !needed {
			continue
		}
		if received[u.UnitRef] != nil {
			return errActivityPrivacy
		}
		exact, e := LoadWork(s, u.Label)
		if e != nil || !equalJSON(u, exact) || !ValidTaskID(exact.Label) || exact.UnitRef != hint.EvalRef(s.Key(), "work-unit", exact.Label) || validateProjectJournal(exact) != nil {
			return errActivityPrivacy
		}
		revisions := []any{}
		for _, r := range exact.Revisions {
			scope, _ := r["scope"].(map[string]any)
			if scope == nil {
				return errActivityPrivacy
			}
			if _, future := scope["verification_intent"]; future {
				return errActivityPrivacy
			}
			revisions = append(revisions, r)
		}
		// Use the existing normal report schema/checker, not a second journal parser.
		builder := report.Builder{Namespace: s.Cfg.ProjectNamespace, Key: s.Key(), WorkUnits: revisions}
		if _, _, e := builder.Build(nil, nil, now); e != nil {
			return errActivityPrivacy
		}
		received[u.UnitRef] = exact
	}
	for unit, epochs := range groups {
		u := received[unit]
		if u == nil {
			return errActivityPrivacy
		}
		sort.Slice(epochs, func(i, j int) bool { return epochs[i].number < epochs[j].number })
		byRevision := map[string]map[string]any{}
		for _, r := range u.Revisions {
			byRevision[str(r["revision_digest"])] = r
		}
		scopeDigest, e := hint.Digest(u.Scope)
		if e != nil {
			return errActivityPrivacy
		}
		var previousRaw, previousWire any
		for i, epoch := range epochs {
			if epoch.number != i+1 {
				return errActivityPrivacy
			}
			events := epoch.body["events"].([]any)
			first := events[0].(map[string]any)
			last := events[len(events)-1].(map[string]any)
			var wantRaw any
			if i > 0 {
				wantRaw, e = hint.Digest(previousRaw)
				if e != nil {
					return errActivityPrivacy
				}
			}
			start := byRevision[str(first["enrollment_start_revision_digest"])]
			frozen := byRevision[str(last["frozen_open_revision_digest"])]
			if first["predecessor_epoch_digest"] != wantRaw || first["original_scope_digest"] != scopeDigest || start == nil || frozen == nil || frozen["state"] != "open" || start["revision"].(float64) > frozen["revision"].(float64) {
				return errActivityPrivacy
			}
			wireEvents := []any{}
			var prev any
			for _, raw := range events {
				ev := raw.(map[string]any)
				out := map[string]any{}
				// Use the SAME closed local event table, but construct ONLY public fields.
				for _, field := range eventFields[str(ev["kind"])] {
					switch field {
					case "prev_digest":
						out[field] = prev
					case "policy_digest":
						out["membership_policy_ref"] = hint.DeriveID(s.Key(), "membership-policy", str(ev[field]))
					case "launch_digest":
						out["launch_configuration_ref"] = activity.ConfigRef(s.Key(), "setting_override", str(ev[field]))
					case "execution_digest":
						out["execution_provenance_ref"] = hint.DeriveID(s.Key(), "execution-provenance", str(ev[field]))
					case "gate_policy_digest":
						out["gate_policy_ref"] = hint.DeriveID(s.Key(), "gate-policy", str(ev[field]))
					case "predecessor_epoch_digest":
						out[field] = nil
						if i > 0 {
							out[field], e = hint.Digest(previousWire)
							if e != nil {
								return errActivityPrivacy
							}
						}
					default:
						out[field] = ev[field]
					}
				}
				prev, e = hint.Digest(out)
				if e != nil {
					return errActivityPrivacy
				}
				wireEvents = append(wireEvents, out)
			}
			body := map[string]any{"unit_ref": unit, "events": wireEvents, "head_digest": prev}
			obs := privacyObservation(epoch.observation)
			obs["membership"] = body
			p.observations[epoch.observation.ObservationID] = obs
			previousRaw, previousWire = epoch.body, body
		}
	}
	return nil
}

// Keep source copying local and typed; raw maps never become a public source declaration.
func decodePrivacySource(m map[string]any, out *activity.Source) error {
	b, e := json.Marshal(m)
	if e != nil {
		return errActivityPrivacy
	}
	if e = json.Unmarshal(b, out); e != nil {
		return errActivityPrivacy
	}
	return nil
}
