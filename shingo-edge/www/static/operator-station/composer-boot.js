// composer-boot.js — the changeover modal's own assets (composer-render.js,
// composer.css, flowspec-data.js), loaded when it is first opened rather than
// at every board's boot.
//
// WHAT THIS SAVES AND WHY IT MATTERS. The station is a Pi on plant WiFi, and
// the board is the screen an operator looks at all shift; the composer is two
// taps away and most shifts never reach it. Boot was parsing about 98 kB of
// composer JS and CSS regardless — composer-render.js, the flowspec block and
// composer.css — before the board drew its first tile.
//
// WHAT STAYS AT BOOT, and it is not an oversight:
//
//   - composer-glyphs.js and composer-glyphs.css, because the board's own
//     changeover card draws the swap-mode glyph.
//   - flow-picture.css, for the style chip's flow panel.
//
// THE MODEL IS DEFERRED TOO, behind its own loader (ensureComposerModel).
// composer-model.js is the largest file the station has, about 45 kB on the
// wire and 137 kB to parse, and the board reads it in two places only: the
// style chip's flow panel (operator-flow.js syncFlowPanel) and the composer.
// Both wait on the one loader below. The picture functions stay synchronous,
// because the Processes page calls them with the model already on window
// (its own classic tag, processes.html); a page whose tag has run the model
// fetches nothing here.
//
// ONE LOAD, HELD. Every entry point calls ensureComposer() or
// ensureComposerModel(); the promise is kept, so a second tap while the first
// is still fetching joins it rather than starting a second fetch, and every
// tap after that is synchronous.

// HASHED FROM THE PAGE'S IMPORT MAP. The station's import map
// (operator-display.html) lists the three files loaded by tag here as well as
// its modules, each plain URL to its hashed one, and a hashed URL is cached
// for good: a tap after a restart that changed nothing costs no request. The
// map is an element in the DOM, so a browser that ignores import maps reads
// the same table. A page with no map, or a file it does not list, gets the
// plain URL, which revalidates by ETag. composer-render.js is a module and is
// not looked up here: its dynamic import names one literal, which the map
// resolves in a browser that has one and which stays plain in one that has
// not, so it is one URL either way, like every other import.
let hashedURLs = null;

function hashed(url) {
    if (hashedURLs === null) {
        hashedURLs = {};
        try {
            const map = document.querySelector('script[type="importmap"]');
            const imports = map && JSON.parse(map.textContent).imports;
            if (imports) hashedURLs = imports;
        } catch (_) {
            // An unreadable map loads every file by its plain URL.
        }
    }
    return hashedURLs[url] || url;
}

let loading = null;

function loadStyle(href) {
    return new Promise(resolve => {
        if (document.querySelector('link[data-composer-asset="' + href + '"]')) { resolve(); return; }
        const link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = hashed(href);
        link.dataset.composerAsset = href;
        // RESOLVE EITHER WAY. A stylesheet that 404s must not stop the
        // composer opening: an unstyled sheet an operator can still read beats
        // a button that does nothing.
        link.addEventListener('load', () => resolve());
        link.addEventListener('error', () => resolve());
        document.head.appendChild(link);
    });
}

// A TAG THAT FAILED IS TAKEN OUT. The check below skips a src whose tag is
// already in <head>, which is right for one that loaded and wrong for one
// that did not: left in, it made every "tap again" skip the file the first tap
// lost (a lost flowspec-data.js opened the composer without the flowspec).
function loadScript(src) {
    return new Promise((resolve, reject) => {
        if (document.querySelector('script[data-composer-asset="' + src + '"]')) { resolve(); return; }
        const s = document.createElement('script');
        s.src = hashed(src);
        s.dataset.composerAsset = src;
        s.addEventListener('load', () => resolve());
        s.addEventListener('error', () => {
            s.remove();
            reject(new Error('failed to load ' + src));
        });
        document.head.appendChild(s);
    });
}

let modelLoading = null;

// ensureComposerModel resolves window.ComposerModel, or null when
// composer-model.js could not be loaded. It is the model's one loader: the
// flow panel waits on it before it draws, and ensureComposer before the
// render module runs.
//
// A FAILED LOAD RESOLVES null AND THE CALLER SAYS SO, in the station's words
// (the flow panel's toast, the changeover's). Clearing the held promise is
// what lets the next tap try again. Nothing retries on its own: a lost request
// waits for a tap, and never becomes one request per view refresh.
export function ensureComposerModel() {
    if (window.ComposerModel) return Promise.resolve(window.ComposerModel);
    if (modelLoading) return modelLoading;
    modelLoading = loadScript('/static/operator-station/composer-model.js')
        .then(() => {
            if (!window.ComposerModel) throw new Error('composer-model.js ran and set no window.ComposerModel');
            return window.ComposerModel;
        })
        .catch(err => {
            modelLoading = null;
            console.error('composer model', err);
            return null;
        });
    return modelLoading;
}

// ensureComposer resolves once window.ComposerUI is ready to be called.
//
// ORDER IS LOAD-BEARING: flowspec-data.js sets window.FLOWSPEC, which
// composer-model.js's init reads for the per-mode rules and every surface
// reads for a field's word. It is awaited before the render module, so a
// composer that opens is a composer that knows what its choreography forbids.
// The model is awaited before it too: composer-render.js draws every screen
// through window.ComposerModel, and a cold #compose= opens one from the
// module's own boot (bootFromHash), with no tap in between.
export function ensureComposer() {
    if (window.ComposerUI) return Promise.resolve(window.ComposerUI);
    if (loading) return loading;
    loading = loadStyle('/static/operator-station/composer.css')
        .then(() => loadScript('/static/operator-station/flowspec-data.js'))
        .then(() => ensureComposerModel())
        .then(model => { if (!model) throw new Error('the composer model did not load'); })
        .then(() => import('/static/operator-station/composer-render.js'))
        .then(() => window.ComposerUI)
        .catch(err => {
            // A failed load leaves ComposerUI undefined and the caller says
            // "tap again" (operator-render's openComposerPicker) — there is no
            // second picker to fall back to any more (pick-a-part U4).
            // Clearing `loading` is what makes the next tap try again: a Pi
            // that lost one request is not left without a changeover until it
            // is rebooted.
            loading = null;
            console.error('composer assets', err);
            return null;
        });
    return loading;
}

// THE SHOTS HARNESS ARRIVES BY URL, not by tap. scripts/composer-shots.sh
// opens /operator/station/N#compose=<screen>;parts=… cold, and
// composer-render.js's own bootFromHash reads it once the module evaluates —
// so the hash has to pull the module in the same way a tap does.
if (/#compose=/.test((window.location && window.location.hash) || '')) ensureComposer();
