// Lineside buckets on the Bins page (moved from /inventory, owner 2026-10-29):
// Core's mirror of the Edge's piles, the bin-level detail beside the bins.
// Active piles count toward a part's in-loop UoP; a stranded row is a count
// anomaly left at a cutover and never counts. Read-only: Core never writes a
// pile. Data: /api/buckets, with updated_at for staleness.

import { apiGet, escapeHtml, timeAgo } from '/static/app.js';
import { formatTime, serverNow } from '/static/shared/utils.js';

const STALE_WARN_MS = 7 * 24 * 3600 * 1000;
const STALE_BAD_MS = 30 * 24 * 3600 * 1000;

function num(n) { return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString(); }

// bucketAgeMs is how long an active pile has gone unchanged. A stranded row
// never changes again, so its age is not a ghost risk.
function bucketAgeMs(b) {
  if (b.state === 'stranded') return 0;
  const t = b.updated_at ? new Date(b.updated_at).getTime() : NaN;
  return isFinite(t) ? (serverNow() - t) : 0;
}

// bucketRowsHtml is the table body, one row per pile; an active pile untouched
// for 30 days is marked stale.
function bucketRowsHtml(buckets) {
  if (!buckets.length) return '<tr><td colspan="5" class="dash-empty">No lineside buckets.</td></tr>';
  return buckets.map((b) => {
    const stranded = b.state === 'stranded';
    const age = bucketAgeMs(b);
    const stale = age > STALE_BAD_MS;
    const ageCls = stale ? 'stale-30' : age > STALE_WARN_MS ? 'stale-7' : '';
    const ageText = b.updated_at ? timeAgo(b.updated_at) : '—';
    const stateCell = stranded
      ? '<span class="badge badge-flagged" title="What was left of the pile when the style changed. Not counted.">count anomaly at cutover</span>'
      : '<span class="badge badge-available">active</span>';
    return '<tr' + (stale ? ' class="row-stale"' : '') + '>'
      + '<td><code>' + escapeHtml(b.node_name || '') + '</code></td>'
      + '<td><code>' + escapeHtml(b.payload_code || '') + '</code></td>'
      + '<td>' + stateCell + '</td>'
      + '<td class="rh-num">' + num(b.qty) + '</td>'
      + '<td class="' + ageCls + '"' + (b.updated_at ? ' title="' + escapeHtml(formatTime(b.updated_at)) + '"' : '')
      + '>' + escapeHtml(ageText) + (stale ? ' · <b>stale</b>' : '') + '</td>'
      + '</tr>';
  }).join('');
}

// staleCount is the head's figure: active piles untouched for 30 days.
function staleCount(buckets) {
  return buckets.filter((b) => bucketAgeMs(b) > STALE_BAD_MS).length;
}

async function loadBuckets() {
  const body = document.getElementById('buckets-body');
  const head = document.getElementById('buckets-stale');
  if (!body) return;
  let buckets;
  try {
    const bk = await apiGet('/api/buckets');
    buckets = Array.isArray(bk) ? bk : (bk && bk.rows) || [];
  } catch (e) {
    body.innerHTML = '<tr><td colspan="5" class="dash-empty">Could not load the lineside buckets — '
      + escapeHtml(String(e.message || e)) + '</td></tr>';
    return;
  }
  body.innerHTML = bucketRowsHtml(buckets);
  const stale = staleCount(buckets);
  if (head) head.textContent = stale ? stale + ' stale' : '';
}

loadBuckets();
