// core-page.js — the helpers a Core admin page holds, as the core-hub surface
// of pins.js runs them. app.js keeps only Core's own verbs (wave C,
// s2-hubs-core), so a page takes the modals, confirm, prompt and toast from
// the shared components and the door verbs from app.js. app.js is imported
// here, not just named, so its import-time work (the bus subscriptions, the
// submit gate, table sort) runs in the page as it does in a browser.
//
// The names are the ones Core's pages import them under.

import { apiDelete, apiGet, apiPost, apiPut } from '/static/app.js';
import { confirm as uiConfirm, confirmWithInput as uiConfirmWithInput, hideModal, prompt as uiPrompt, showModal } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';

export { apiDelete, apiGet, apiPost, apiPut, hideModal, showModal, toast, uiConfirm, uiConfirmWithInput, uiPrompt };
