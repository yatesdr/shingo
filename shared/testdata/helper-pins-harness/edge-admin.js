// edge-admin.js — what an Edge admin page takes under each helper name since
// wave C: the Edge hub (loaded first, as header.html loads it, for what it
// does on import, and its api) and the shared components each page imports
// from their own files. pins.js's edge-hub surface is this module, so the
// edge-hub goldens read what an Edge admin page runs; hubs.txt reads the hub
// itself.
//
// installBackdropClose is a no-op, as on the components' own surface: no Edge
// admin page calls it since the hub stopped, and layer.js needs no install
// (showModal reads data-backdrop-close when it opens the overlay). The modal
// case list still calls it, as a page once did.

import { api } from '/static/js/shingoedge.js';
export { api };
export { showModal, hideModal, confirm, prompt } from '/static/shared/layer.js';
export { toast } from '/static/shared/notice.js';

export function installBackdropClose() {}
