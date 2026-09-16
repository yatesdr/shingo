// desktop-bodies.js — every request body the Processes page sends, in one
// place, built from explicit inputs and nothing else.
//
// WHY THIS FILE EXISTS. Every handler the desktop writes to WRITES EVERY FIELD
// IT DECODES. There is no partial update on the process PUT, the style PUT or
// either station handler: a field left out of the body is a field set to its
// zero value. U9d wired four of them from bodies that named only what the
// screen had changed, and every one was a data-loss bug —
//
//   - the settings save left out production_state and group_id. The direction
//     of the first one is NOT what 82f05880's message said: store's Update
//     coerces an empty productionState to "active_production"
//     (store/processes/processes.go:102), so the missing field did not blank
//     the column, it FORCED it — and the value it destroys is
//     'changeover_active'. Saving an unrelated setting on D5 while the floor
//     was mid-changeover told the rest of the system the changeover was over.
//     The missing group_id did clear the FK: nil is the handler's "Ungrouped";
//   - rename and Expected CATID left out process_id (a 400) and description
//     (blanked, had the 400 not fired first);
//   - the operator-screen edit left out code, area_label, sequence,
//     controller_node_id and device_mode, and `enabled` defaulting to false
//     switched a live HMI off to fix a typo in its note.
//
// The fix in each case is the same shape — start from the row as it stands and
// overlay the change — and the reason to put the eight builders here rather
// than beside the eight call sites is that a handler-level test can then post
// EXACTLY what the page posts. www/desktop_write_paths_test.go runs this file
// under node, takes the bodies it produces, sends them to the real router, and
// reads the rows back. A builder that stops naming a column is a red test
// rather than a field quietly zeroed in production.
//
// NOTHING HERE READS PAGE STATE. Every input is an argument, which is what
// makes the file runnable outside a browser. Classic script plus CommonJS, the
// way composer-model.js is loaded, so the page gets it on window and the test
// harness can require() it.

'use strict';

// processSettings — PUT /api/processes/{id}, D5's Save settings.
//
// process is the row as it stands; draft is D5's editor state.
//
// production_state is NOT in the draft and never becomes editable here: P0
// owns the display and the changeover owns the transitions, so it rides along
// unchanged. It has to be SPOKEN, though, and not merely left out: the store
// reads an empty one as "active_production", so omitting it forces every saved
// process into production — ending a live changeover on the strength of an
// edit to a counter tag.
//
// group_id is the draft's, because the group picker is on this screen, and
// null is the handler's "Ungrouped" — which is why sending nothing ungrouped
// every process that was saved.
function processSettings(process, draft) {
    return {
        name: draft.name || '',
        description: draft.description || '',
        counter_plc_name: draft.counter_plc_name || '',
        counter_tag_name: draft.counter_tag_name || '',
        counter_enabled: !!draft.counter_enabled,
        changeover_auto_arm: draft.changeover_auto_arm || '',
        production_state: (process && process.production_state) || '',
        group_id: draft.group_id || null,
    };
}

// processCreate — POST /api/processes, the Add-process sheet's first write.
//
// EVERY FIELD THE HANDLER DECODES, and here that is a choice rather than the
// rescue it is on the PUT: an INSERT with a field left out gets the column's
// zero, which for a brand-new process is the right value anyway. Spelling them
// is what makes the defaults READABLE — an empty counter and an unset auto-arm
// are decisions this sheet is deliberately not asking about (they belong in
// Settings, beside the press that is already running), not fields nobody
// thought of.
//
// production_state is 'active_production' because that is the only state a new
// process can be in: store's Create coerces an empty one to it
// (store/processes/processes.go:89), the other value in the tree is
// 'changeover_active', and a changeover owns that transition. The retired Add
// Process modal sent the same.
//
// group_id is null for Ungrouped — the handler's own spelling, and the reason
// sending nothing ungrouped every process that was saved.
function processCreate(draft) {
    return {
        name: (draft && draft.name) || '',
        description: (draft && draft.description) || '',
        production_state: 'active_production',
        counter_plc_name: '',
        counter_tag_name: '',
        counter_enabled: false,
        changeover_auto_arm: 'auto',
        group_id: (draft && draft.group_id) || null,
    };
}

// stationNodes — PUT /api/operator-stations/{id}/claimed-nodes.
//
// THE WHOLE LIST, because SetNodes is a set-to: a name left out is a position
// DETACHED from this station, which is the correct semantics for an edit and
// the reason this body is never a delta.
//
// Trimmed and deduplicated here as well as in the service, so the body says
// exactly what the engineer picked rather than leaving the page and the store
// with two ideas of it.
function stationNodes(names) {
    const out = [];
    for (const raw of names || []) {
        const name = String(raw || '').trim();
        if (name && out.indexOf(name) < 0) out.push(name);
    }
    return { nodes: out };
}

// processGate — PATCH /api/processes/{id}. The only PATCHable field, flipped on
// its own after a review of the routing set and never as a side effect of a
// save (see apiPatchProcess).
function processGate(enabled) {
    return { flow_composer_enabled: !!enabled };
}

// styleWrite — PUT /api/styles/{id}, for both the rename and the Expected
// CATID sheet. apiUpdateStyle refuses a zero process_id and writes description,
// so the style as it stands is the base and the sheet's one field is the
// overlay.
//
// expected_catid falls back to `catid` because the list read and the composer
// read spell it differently on the two blocks the page holds.
function styleWrite(style, processID, change) {
    const base = {
        name: (style && style.name) || '',
        description: (style && style.description) || '',
        process_id: (style && style.process_id) || processID,
        expected_catid: (style && (style.expected_catid || style.catid)) || '',
    };
    return Object.assign(base, change || {});
}

// styleCreate — POST /api/styles, the rail's "+ New part flow".
//
// process_id is REFUSED WHEN ZERO by apiCreateStyle, so it is an argument
// rather than something a caller may forget, and expected_catid rides along
// because the handler has its own setter for it and a style named without one
// is a style the PLC can never match to a scan. description is named for the
// same reason every other builder names it: the handler decodes it.
function styleCreate(name, processID, catid) {
    return {
        name: name || '',
        description: '',
        process_id: Number(processID) || 0,
        expected_catid: catid || '',
    };
}

// processGroupCreate — POST /api/process-groups, P0's "Add group".
//
// A group is pure taxonomy for the list — nothing reads it — so the two fields
// the handler decodes are the whole of it. A duplicate name comes back 409 by
// name, which is why the sheet shows the refusal instead of guarding.
function processGroupCreate(name, description) {
    return { name: name || '', description: description || '' };
}

// styleClone — POST /api/styles/{id}/clone. The handler takes the new name and
// copies the rest from the source, so the name is the whole body.
function styleClone(name) {
    return { name: name || '' };
}

// processActiveStyle — PUT /api/processes/{id}/active-style, "Mark as running".
// An admin correction to shingo's record of what the press is already
// stamping; no robot moves and no other field is touched.
function processActiveStyle(styleID) {
    return { style_id: Number(styleID) };
}

// stationWrite — POST /api/operator-stations and PUT /api/operator-stations/{id}.
//
// BOTH HANDLERS DECODE THE WHOLE StationInput. An edit therefore starts from
// the station as it stands: code, area_label, sequence, controller_node_id and
// device_mode are not on the sheet and must survive it, and `enabled` has to be
// spoken because a missing bool decodes false and would switch a live HMI off.
// A new screen takes the defaults the retired page sent for one.
function stationWrite(station, processID, editing, change) {
    const base = editing ? {
        process_id: (station && station.process_id) || processID,
        code: (station && station.code) || '',
        name: (station && station.name) || '',
        note: (station && station.note) || '',
        area_label: (station && station.area_label) || '',
        sequence: (station && station.sequence) || 0,
        controller_node_id: (station && station.controller_node_id) || '',
        device_mode: (station && station.device_mode) || 'fixed_hmi',
        enabled: !!(station && station.enabled),
    } : {
        process_id: processID, code: '', name: '', note: '', area_label: '',
        sequence: 0, controller_node_id: '', device_mode: 'fixed_hmi', enabled: true,
    };
    return Object.assign(base, change || {});
}

// routingEnable — PATCH /api/processes/{id}/routing-nodes/{rowID}.
//
// `enabled` and nothing else. THE SWITCH IS THE APPROVAL and the SERVER
// records it: enabling a backfilled row stamps origin='engineer' and the
// session user, so a page sending an origin of its own would be a page
// deciding authorship. It used to send one, and the handler dropped it.
function routingEnable(enabled) {
    return { enabled: !!enabled };
}

// routingAdd — POST /api/processes/{id}/routing-nodes. origin and called_by are
// server-stamped and the input type does not accept them from a body.
//
// SEQUENCE IS NOT DECORATION. composer-model's defaultRouting picks a cell's
// default source and destination by LOWEST SEQUENCE within the role, so it is
// the mechanism that decides which name a new position opens on. Every row
// this page wrote sent nothing and got 0, and the backfill inserts 0 too, so
// every row in every role tied at zero and the documented ordering was inert —
// the default fell out of whatever order SQLite returned.
//
// `after` is how many rows the role already has, so a name lands at the end of
// its role and the first one added stays the default. It is an argument rather
// than page state, like every other input in this file.
function routingAdd(name, role, after) {
    return {
        core_node_name: name, role: role, label: name, enabled: true,
        sequence: Number(after) > 0 ? Number(after) + 1 : 1,
    };
}

// flowPresetCreate — POST /api/processes/{id}/presets, the naming modal.
//
// EXACTLY ONE SOURCE, and the handler refuses both or neither: a shape comes
// from a style's saved flow (D1's `Save as preset…`) or from one of the offered
// candidate shapes, and those are different acts. Naming a candidate also
// stamps the styles that already run it.
//
// created_by is NOT here and never becomes a field: the session is the author,
// the same rule the routing set's origin and the flow save's source are held
// to. A page that named an author would be a page deciding authorship.
function flowPresetCreate(name, from) {
    const body = { name: name || '' };
    if (from && from.styleID) body.from_style_id = Number(from.styleID);
    else if (from && from.shapeKey) body.from_candidate_shape = String(from.shapeKey);
    return body;
}

// flowPresetApplySave — POST /api/processes/{id}/flow/save, one row of the
// apply modal.
//
// THE SAME DOOR AS D1'S OWN SAVE, with two columns added. Apply is not an
// endpoint (handlers_flow_presets.go): a preset reaches a style through
// flow/preview and then this, on that preview's fingerprint, so a preset can
// never change a style without a preview on screen.
//
// BOTH PROVENANCE FIELDS OR NEITHER. The handler refuses an id without a
// version — provenance nobody can read is worse than none — so they are
// written from one object and cannot be half-sent.
function flowPresetApplySave(styleID, cells, fingerprint, stationID, preset) {
    const body = {
        to_style_id: Number(styleID),
        cells: cells,
        fingerprint: fingerprint || '',
        station_id: Number(stationID) || 0,
    };
    if (preset && preset.id && preset.version) {
        body.source_preset_id = Number(preset.id);
        body.source_preset_version = Number(preset.version);
    }
    return body;
}

// ── the apply loop's two decisions ───────────────────────────────────────────
//
// NOT A BODY, AND HERE ANYWAY. runPresetApply is the page's only unguarded
// state machine: it walks every ticked style, saves each one, and reacts to
// what comes back. The walk itself needs a DOM and a network; the two
// DECISIONS inside it do not, and they are the two that were wrong.
//
// This file is where the page's logic goes to be testable — the same reason
// the eight body builders are here.

// applyOrder is the order a preset apply saves its styles in: the rail's,
// with the RUNNING style last.
//
// The fingerprint covers BOTH sides — the target's claims and the claims of
// the style the press is running — because the planner switches on the
// outgoing claim's mode. So saving the running style moves the from-side under
// every other row's fingerprint at once, and an apply that reached it in rail
// order 409-cascaded through everything after it. Last, it invalidates nothing
// that still has to be written.
function applyOrder(styles, runningStyleID) {
    const all = (styles || []).slice();
    if (!runningStyleID) return all;
    return all.filter(s => s.id !== runningStyleID).concat(all.filter(s => s.id === runningStyleID));
}

// applyOutcome says what a save's response means for one row: retry it once
// after a fresh preview, record it saved, or record the refusal by name.
//
// A 409 IS THE ORDINARY OUTCOME OF THE ROW BEFORE THIS ONE, not an engineer's
// mistake — the save that just landed moved the rows this one was fingerprinted
// against. The loop used to stop there and leave the row unwritten under a
// sentence about previewing, so the engineer re-ran the whole apply to pick it
// up.
//
// ONCE, and `attempt` is what makes that true: a second 409 means something is
// genuinely moving the rows underneath — another session, or a changeover —
// and retrying into that is a loop rather than a fix.
function applyOutcome(status, body, attempt) {
    if (status === 409 && (attempt || 1) < 2) {
        return { retry: true, text: 'the flow changed since you previewed — previewing again' };
    }
    if (status >= 200 && status < 300) {
        return { retry: false, ok: true, text: 'saved' };
    }
    return {
        retry: false, ok: false,
        text: (body && body.error) || 'refused (' + status + ')',
    };
}

// saveOutcome says what a flow/save response means for the Save button, as one
// decision with one set of branches.
//
// EVERY REFUSAL BY NAME. The page used to handle 409 and 422 and then redraw
// the bar saying nothing at all for anything else, so a 403 (the flow-composer
// gate), a 400 (an unknown station, a half-written preset provenance) and a 500
// all read to the engineer as a Save button that did not work. The server names
// its refusals — that is what the sentences on flow/save are for.
//
// 409 IS TWO THINGS. `stale` means the rows moved under the fingerprint, which
// is ordinary and self-healing: re-preview and say so. A 409 without it is a
// rule refusing the save (the running-position move is the one that matters),
// and that one has a sentence naming the style and the positions, which must
// reach the screen instead of being flattened into "checking it again".
//
// Here rather than in the page because the page is what a screenshot cannot
// check: this is the same lift applyOutcome got, for the same reason, and
// apply-loop.test.js drives both.
function saveOutcome(status, body) {
    const b = body || {};
    if (status === 409) {
        return b.stale
            ? { kind: 'stale', text: 'The flow changed since you previewed — checking it again' }
            : { kind: 'refused', text: b.error || 'Refused' };
    }
    if (status === 422) return { kind: 'findings', text: '' };
    if (status >= 200 && status < 300) return { kind: 'saved', text: '' };
    return { kind: 'refused', text: b.error || ('The save was refused (' + status + ')') };
}

// EXPORTED WITHOUT A GLOBAL LEXICAL NAME. Two classic scripts on one page
// share one global lexical environment, so a top-level `const api` in each is
// a redeclaration: the SECOND script dies before its last line and its global
// is never set. That is exactly what processes.html was doing —
// composer-model.js and desktop-bodies.js both declared `const api`, so
// window.DesktopBodies was undefined and every write button on the Processes
// page threw. The block is a function scope now, in both files, and
// www/classic_script_globals_test.go proves no page can make that mistake
// again.
(function () {
    const api = {
        processCreate, processSettings, processGate, processGroupCreate,
        styleCreate, styleWrite, styleClone,
        processActiveStyle, stationWrite, stationNodes, routingEnable, routingAdd,
        flowPresetCreate, flowPresetApplySave,
        applyOrder, applyOutcome, saveOutcome,
    };

    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.DesktopBodies = api;
})();
