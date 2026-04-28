// 3-state theme toggle: light -> dark -> system -> light.
// Stored value in localStorage: 'light' | 'dark' | absent (system).
//
// Shared verbatim between shingo-core and shingo-edge. If you edit this
// file, edit both copies together.
(function() {
  function getStoredTheme() {
    return localStorage.getItem('theme');
  }

  function getEffectiveTheme() {
    var stored = getStoredTheme();
    if (stored === 'light' || stored === 'dark') return stored;
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  function applyTheme() {
    document.documentElement.dataset.theme = getEffectiveTheme();
    var btn = document.querySelector('.theme-toggle');
    if (!btn) return;
    var stored = getStoredTheme();
    if (stored === 'dark') {
      btn.textContent = '\u263D'; // moon
      btn.title = 'Theme: dark (click for system)';
    } else if (stored === 'light') {
      btn.textContent = '\u2600'; // sun
      btn.title = 'Theme: light (click for dark)';
    } else {
      btn.textContent = '\u25D0'; // half-circle (system)
      btn.title = 'Theme: system (click for light)';
    }
  }

  window.toggleTheme = function() {
    var stored = getStoredTheme();
    if (stored === 'light') {
      localStorage.setItem('theme', 'dark');
    } else if (stored === 'dark') {
      localStorage.removeItem('theme');
    } else {
      localStorage.setItem('theme', 'light');
    }
    applyTheme();
  };

  document.addEventListener('DOMContentLoaded', function() {
    applyTheme();
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
      if (!getStoredTheme()) applyTheme();
    });
  });
})();
