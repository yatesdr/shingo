// ask-act.js — a desktop page's action POST to a door that may answer while
// its ask is still out with Core.
//
// A LOAD, a REQUEST EMPTY or a hold clear goes to Core as an ask. When Core
// has not answered within the Edge's wait, the door answers 202
// {pending: true, notice}: the act was sent and is not known to be done.
// api.post (shingoedge.js) reads every 2xx as done, which is how the
// Production page said "Bin loaded" over an act Core had not answered.
//
// postAct is the station's rule (operator-util.js postAction) for the desktop
// pages: a 202 pending answer is returned as {pending, notice} for the caller
// to show, never a success, and the same action URL is held for ASK_HOLD_MS —
// a second tap answers the notice and sends nothing. Core's answer redraws the
// page when it lands. Any other answer is api.post's: the decoded body, or the
// refusal's sentence thrown.

// ASK_HOLD_MS is the station's hold (operator-util.js): the ask's lifetime at
// Core (protocol.AskTTL, 25 s) and the Edge's slack past it (engine
// askForgetSlack, 10 s). Pinned equal by ask-pending.test.js.
export const ASK_HOLD_MS = 35000;

// heldActs holds, by action URL, the acts sent and not yet answered.
const heldActs = new Map();

export async function postAct(url, body) {
    const held = heldActs.get(url);
    if (held && held.until > Date.now()) {
        return { pending: true, notice: held.notice };
    }
    heldActs.delete(url);
    const res = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body || {})
    });
    if (res.status === 202) {
        let parsed = null;
        try { parsed = await res.clone().json(); } catch (e) { parsed = null; }
        if (parsed && parsed.pending) {
            heldActs.set(url, { notice: parsed.notice, until: Date.now() + ASK_HOLD_MS });
            return parsed;
        }
    }
    if (!res.ok) {
        const text = await res.text();
        let parsed = null;
        try { parsed = JSON.parse(text); } catch (e) { parsed = null; }
        throw (parsed && parsed.error) || text;
    }
    return res.json();
}
