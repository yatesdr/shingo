// surface.js — the shared components as a hub will hand them to a page once
// wave C swaps each hub's names onto them: what cases.js runs the helper pins'
// case lists against.
//
// installBackdropClose is a no-op here because the layer needs no install: an
// overlay marked data-backdrop-close is read when showModal opens it. The
// modal case list still calls it, as a page does today.

export { showModal, hideModal, confirm, prompt, confirmWithInput } from '/static/shared/layer.js';
export { toast } from '/static/shared/notice.js';
export { send, http } from '/static/shared/http.js';

export function installBackdropClose() {}
