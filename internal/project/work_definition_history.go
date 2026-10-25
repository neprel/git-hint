package project

// Only bounded received local history is inspected. Inspected absence never
// proves absence of deleted, unreceived or outside work. Nothing is repaired.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runjournal"
	"github.com/neprel/git-hint/internal/hint"
)

func definitionHistory(r *definitionStateRead, label, unit string) ([]string, error) {
	absent := []string{}
	tasks := map[string]string{}
	units := map[string]*WorkUnit{}
	dirs := map[string]*WorkUnit{}
	optionalDir := func(kind string, parts ...string) ([]fs.DirEntry, error) {
		es, e := r.dir(parts...)
		if errors.Is(e, fs.ErrNotExist) {
			absent = append(absent, kind)
			return nil, nil
		}
		return es, e
	}
	es, e := optionalDir("work_directory", "state", "work")
	if e != nil {
		return nil, e
	}
	for _, entry := range es {
		if !entry.Type().IsRegular() || !batchID.MatchString(strings.TrimSuffix(entry.Name(), ".json")) || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, ErrWorkDefinition
		}
		raw, e := r.read(definitionMetadataMax, "state", "work", entry.Name())
		if e != nil {
			return nil, e
		}
		var u WorkUnit
		if definitionDecode(raw, &u) != nil || !ValidTaskID(u.Label) || entry.Name() != workFile(r.s, u.Label) || u.UnitRef != hint.EvalRef(r.s.Key(), "work-unit", u.Label) || len(u.Revisions) == 0 || (u.State != "open" && u.State != "closed") || u.Scope == nil {
			return nil, ErrWorkDefinition
		}
		if u.UnitRef == unit {
			return nil, ErrWorkDefinition
		}
		scope, e := hint.Digest(plainOf(u.Scope))
		if e != nil {
			return nil, e
		}
		var previous any
		for i, rev := range u.Revisions {
			n, ok := rev["revision"].(float64)
			if !ok || n != float64(i+1) || rev["unit_ref"] != u.UnitRef || rev["origin"] != "project" || rev["scope_digest"] != scope || rev["predecessor"] != previous {
				return nil, ErrWorkDefinition
			}
			body := map[string]any{}
			for k, v := range rev {
				if k != "revision_digest" {
					body[k] = v
				}
			}
			dg, e := hint.Digest(body)
			if e != nil || rev["revision_digest"] != dg {
				return nil, ErrWorkDefinition
			}
			sd, e := hint.Digest(rev["scope"])
			if e != nil || sd != scope {
				return nil, ErrWorkDefinition
			}
			previous = dg
			if i > 0 && u.Revisions[i-1]["state"] == "closed" {
				return nil, ErrWorkDefinition
			}
		}
		if u.Revisions[len(u.Revisions)-1]["state"] != u.State {
			return nil, ErrWorkDefinition
		}
		for _, a := range u.Attempts {
			if !batchID.MatchString(a.Ref) {
				return nil, ErrWorkDefinition
			}
			for _, task := range append(append([]string{}, a.Tasks...), a.Late...) {
				if !batchID.MatchString(task) || (tasks[task] != "" && tasks[task] != u.UnitRef) {
					return nil, ErrWorkDefinition
				}
				tasks[task] = u.UnitRef
			}
		}
		if units[u.UnitRef] != nil {
			return nil, ErrWorkDefinition
		}
		copy := u
		units[u.UnitRef] = &copy
		dirs[hint.DeriveID(r.s.Key(), "membership-file", u.Label)] = &copy
	}
	es, e = optionalDir("membership_directory", "state", membershipDir)
	if e != nil {
		return nil, e
	}
	own := hint.DeriveID(r.s.Key(), "membership-file", label)
	for _, dir := range es {
		if !dir.IsDir() || dir.Name() == own || dirs[dir.Name()] == nil {
			return nil, ErrWorkDefinition
		}
		files, e := r.dir("state", membershipDir, dir.Name())
		if e != nil {
			return nil, e
		}
		epochsSeen := map[int]bool{}
		for _, entry := range files {
			if !entry.Type().IsRegular() {
				return nil, ErrWorkDefinition
			}
			raw, e := r.read(maxMembershipBytes, "state", membershipDir, dir.Name(), entry.Name())
			if e != nil {
				return nil, e
			}
			if match := epochFile.FindStringSubmatch(entry.Name()); match != nil {
				v, e := parseEpoch(raw)
				if e != nil {
					return nil, e
				}
				n, ok := evInt(v.open, "epoch")
				if !ok || epochsSeen[n] {
					return nil, ErrWorkDefinition
				}
				epochsSeen[n] = true
				dg, _ := hint.Digest(plainOf(dirs[dir.Name()].Scope))
				if v.open["original_scope_digest"] != dg {
					return nil, ErrWorkDefinition
				}
			} else if m := regexp.MustCompile(`^policy-([1-9][0-9]{0,5})\.json$`).FindStringSubmatch(entry.Name()); m != nil {
				var policy MembershipPolicy
				if definitionDecode(raw, &policy) != nil || policy.problem() != nil {
					return nil, ErrWorkDefinition
				}
			} else if strings.HasPrefix(entry.Name(), "configuration-") && strings.HasSuffix(entry.Name(), ".json") {
				dg := "sha256:" + strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "configuration-"), ".json")
				var cfg activity.Configuration
				if !digestPattern.MatchString(dg) || definitionDecode(raw, &cfg) != nil || !definitionConfigurationValid(cfg) || cfg.Digest() != dg {
					return nil, ErrWorkDefinition
				}
			} else if m := regexp.MustCompile(`^sealed-([1-9][0-9]{0,5})\.json$`).FindStringSubmatch(entry.Name()); m != nil {
				var batch sealedBatch
				if definitionDecode(raw, &batch) != nil || len(batch.Observations) != 1 || batch.Observations[0].Membership == nil || batch.Observations[0].Membership.UnitRef != dirs[dir.Name()].UnitRef {
					return nil, ErrWorkDefinition
				}
				b := activityBatch{Version: 1, Source: batch.Source, Observations: batch.Observations}
				b.Digest, _ = activityBatchDigest(b)
				if validateActivityBatch(r.s, b) != nil {
					return nil, ErrWorkDefinition
				}
			} else {
				return nil, ErrWorkDefinition
			}

		}
		for n := 1; n <= len(epochsSeen); n++ {
			if !epochsSeen[n] {
				return nil, ErrWorkDefinition
			}
			raw, e := r.read(maxMembershipBytes, "state", membershipDir, dir.Name(), epochName(n))
			if e != nil {
				return nil, e
			}
			v, e := parseEpoch(raw)
			if e != nil {
				return nil, e
			}
			raw, e = r.read(maxMembershipBytes, "state", membershipDir, dir.Name(), policyName(n))
			var policy MembershipPolicy
			if e != nil || definitionDecode(raw, &policy) != nil || policy.problem() != nil {
				return nil, ErrWorkDefinition
			}
			dg, _ := hint.Digest(policy)
			if v.open["policy_digest"] != dg {
				return nil, ErrWorkDefinition
			}
			for _, enroll := range v.exec {
				name := configName(evStr(enroll, "planned_configuration_digest"))
				raw, e = r.read(definitionMetadataMax, "state", membershipDir, dir.Name(), name)
				var cfg activity.Configuration
				if e != nil || definitionDecode(raw, &cfg) != nil || !definitionConfigurationValid(cfg) || cfg.Digest() != evStr(enroll, "planned_configuration_digest") {
					return nil, ErrWorkDefinition
				}
			}
			if v.sealed {
				if _, e = r.read(maxMembershipBytes, "state", membershipDir, dir.Name(), sealedName(n)); e != nil {
					return nil, ErrWorkDefinition
				}
			}
		}
	}
	sources := map[string]activity.Source{}
	raw, e := r.read(definitionMetadataMax, "state", "activity", sourcesFile)
	if errors.Is(e, fs.ErrNotExist) {
		absent = append(absent, "activity_sources")
	} else if e != nil {
		return nil, e
	} else if definitionDecode(raw, &sources) != nil {
		return nil, ErrWorkDefinition
	}
	var input bytes.Buffer
	raw, e = r.read(16<<20, "state", "activity", spoolFile)
	if errors.Is(e, fs.ErrNotExist) {
		absent = append(absent, "activity_spool")
	} else if e != nil {
		return nil, e
	} else {
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			return nil, ErrWorkDefinition
		}
		for _, line := range bytes.Split(raw, []byte{'\n'}) {
			if len(line) > 0 && !uniqueBatchJSON(line) {
				return nil, ErrWorkDefinition
			}
		}
		input.Write(raw)
	}
	es, e = optionalDir("activity_batches", "state", "activity", activityBatchDir)
	if e != nil {
		return nil, e
	}
	batchObs := map[string]bool{}
	for _, entry := range es {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, ErrWorkDefinition
		}
		raw, e := r.read(16<<20, "state", "activity", activityBatchDir, entry.Name())
		if e != nil {
			return nil, e
		}
		b, e := decodeActivityBatch(r.s, entry.Name(), raw)
		if e != nil {
			return nil, e
		}
		if old, ok := sources[b.Source.SourceID]; ok && !equalJSON(old, b.Source) {
			return nil, ErrWorkDefinition
		}
		sources[b.Source.SourceID] = b.Source
		for _, o := range b.Observations {
			if batchObs[o.ObservationID] {
				return nil, ErrWorkDefinition
			}
			batchObs[o.ObservationID] = true
			line, _ := json.Marshal(o)
			input.Write(line)
			input.WriteByte('\n')
		}
	}
	observations, damage, e := parseSpoolInput(&input)
	if e != nil || damage.Any() {
		return nil, ErrWorkDefinition
	}
	bySource := map[string][]activity.Observation{}
	for _, o := range observations {
		src, ok := sources[o.SourceID]
		if !ok {
			return nil, ErrWorkDefinition
		}
		bySource[o.SourceID] = append(bySource[o.SourceID], o)
		if o.ModelLedger != nil && o.ModelLedger.Binding.UnitRef == unit || o.Membership != nil && o.Membership.UnitRef == unit {
			return nil, ErrWorkDefinition
		}
		if o.Membership != nil && units[o.Membership.UnitRef] == nil {
			return nil, ErrWorkDefinition
		}
		if o.ModelLedger != nil && o.ModelLedger.Binding.Origin == "project" && units[o.ModelLedger.Binding.UnitRef] == nil {
			return nil, ErrWorkDefinition
		}
		// Unattributed agent history can belong to the requested/deleted unit; do not
		// turn missing ownership into a pre-work claim. Ordinary collector facts remain.
		if src.Agent != "none" && src.Agent != "" && o.Membership == nil && o.ModelLedger == nil && tasks[o.Correlation["task_id"]] == "" {
			return nil, ErrWorkDefinition
		}
	}
	for id, src := range sources {
		if id != src.SourceID || !batchID.MatchString(id) {
			return nil, ErrWorkDefinition
		}
		b := activityBatch{Version: 1, Source: src, Observations: bySource[id]}
		if b.Observations == nil {
			b.Observations = []activity.Observation{}
		}
		b.Digest, _ = activityBatchDigest(b)
		if validateActivityBatch(r.s, b) != nil {
			return nil, ErrWorkDefinition
		}
	}
	// The existing controlled-run index is another received ownership source. A
	// malformed or matching record refuses; nothing is inferred from a missing run.
	es, e = optionalDir("controlled_run_index", "state", "evals", "work")
	if e != nil {
		return nil, e
	}
	controlledOwners := map[string]string{}
	for _, entry := range es {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			return nil, ErrWorkDefinition
		}
		raw, e := r.read(16<<20, "state", "evals", "work", entry.Name())
		if e != nil {
			return nil, e
		}
		received, e := runjournal.Parse(r.ctx, raw, entry.Name(), r.s.Key())
		if e != nil {
			return nil, ErrWorkDefinition
		}
		for identity, owner := range received.Ownership() {
			if identity == unit || units[identity] != nil || tasks[identity] != "" || (controlledOwners[identity] != "" && controlledOwners[identity] != owner) {
				return nil, ErrWorkDefinition
			}
			controlledOwners[identity] = owner
		}
	}

	sort.Strings(absent)
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	return absent, nil
}
