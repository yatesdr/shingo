// composer-boot.js — the composer's own assets, loaded when it is first
// opened rather than at every board's boot.
//
// WHAT THIS SAVES AND WHY IT MATTERS. The station is a Pi on plant WiFi, and
// the board is the screen an operator looks at all shift; the composer is two
// taps away and most shifts never reach it. Boot was parsing about 98 kB of
// composer JS and CSS regardless — composer-render.js, the flowspec block and
// composer.css — before the board drew its first tile.
//
// WHAT STAYS AT BOOT, and it is not an oversight:
//
//   - composer-model.js, because operator-flow.js asks it for the read-only
//     picture's sentences (sentencesFromView) the moment the style chip's
//     panel is opened, and that panel is part of the board.
//   - composer-glyphs.js and composer-glyphs.css, because the board's own
//     changeover card draws the swap-mode glyph.
//   - flow-picture.css, for the same panel.
//
// ONE LOAD, HELD. Every entry point calls ensureComposer(); the promise is
// kept, so a second tap while the first is still fetching joins it rather than
// starting a second fetch, and every tap after that is synchronous.

// VERSIONED FROM THIS MODULE'S OWN URL. The template stamps ?v= on the script
// tag that pulled this in, and import.meta.url carries it — so the deferred
// files are busted by exactly the same token as the ones in the <head>,
// without a second mechanism and without a global for the JS to read.
const VERSION = (() => {
    try {
        return new URL(import.meta.url).search || '';
    } catch (_) {
        return '';
    }
})();

let loading = null;

function loadStyle(href) {
    return new Promise(resolve => {
        if (document.querySelector('link[data-composer-asset="' + href + '"]')) { resolve(); return; }
        const link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = href + VERSION;
        link.dataset.composerAsset = href;
        // RESOLVE EITHER WAY. A stylesheet that 404s must not stop the
        // composer opening: an unstyled sheet an operator can still read beats
        // a button that does nothing.
        link.addEventListener('load', () => resolve());
        link.addEventListener('error', () => resolve());
        document.head.appendChild(link);
    });
}

function loadScript(src) {
    return new Promise((resolve, reject) => {
        if (document.querySelector('script[data-composer-asset="' + src + '"]')) { resolve(); return; }
        const s = document.createElement('script');
        s.src = src + VERSION;
        s.dataset.composerAsset = src;
        s.addEventListener('load', () => resolve());
        s.addEventListener('error', () => reject(new Error('failed to load ' + src)));
        document.head.appendChild(s);
    });
}

// ensureComposer resolves once window.ComposerUI is ready to be called.
//
// ORDER IS LOAD-BEARING: flowspec-data.js sets window.FLOWSPEC, which
// composer-model.js's init reads for the per-mode rules and every surface
// reads for a field's word. It is awaited before the render module, so a
// composer that opens is a composer that knows what its choreography forbids.
export function ensureComposer() {
    if (window.ComposerUI) return Promise.resolve(window.ComposerUI);
    if (loading) return loading;
    loading = loadStyle('/static/operator-station/composer.css')
        .then(() => loadScript('/static/operator-station/flowspec-data.js'))
        .then(() => import('/static/operator-station/composer-render.js' + VERSION))
        .then(() => window.ComposerUI)
        .catch(err => {
            // A failed load leaves ComposerUI undefined, and the caller falls
            // back to the board's own flat picker — which is a worse screen
            // and is still a screen. Clearing `loading` lets the next tap try
            // again: a Pi that lost its connection for one request should not
            // be left without a changeover picker until it is rebooted.
            loading = null;
            console.error('composer assets', err);
            return null;
        });
    return loading;
}

// THE SHOTS HARNESS ARRIVES BY URL, not by tap. scripts/composer-shots.sh
// opens /operator/station/N#compose=<style>;state=S7 cold, and
// composer-render.js's own bootFromHash reads it once the module evaluates —
// so the hash has to pull the module in the same way a tap does.
if (/#compose=/.test((window.location && window.location.hash) || '')) ensureComposer();
