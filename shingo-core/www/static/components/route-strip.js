// route-strip.js — an order's route as the boards draw it (the strip) and as
// the order pop-up lists it (the timeline), from what the server sends:
// route (domain.RouteStops, every stop with its state) and route_view
// (domain.RouteViewOf, the strip laid out). Nothing here derives a stop or a
// state. The order-route partial (templates/partials/orders-rows.html) draws
// the same strip server side, and TestOrderRouteRuleMatchesThePopup holds the
// three to the rule. Colours and shapes are route-strip.css's, from tokens: a
// state is a class, and the robot on its way is the separator before the stop
// it heads to (route_view.heading), classed route-heading.

import { escapeHtml } from '/static/shared/utils.js';

// routeStateLabel is a state in words, for the strip's title and the
// timeline; a stop still to come says nothing. Go's routeStateWords
// (www/helpers.go) is the same table.
export function routeStateLabel(state) {
  switch (state) {
    case 'passed': return 'passed';
    case 'left': return 'left';
    case 'here': return 'robot here';
    case 'next': return 'next';
    case 'final': return 'final';
  }
  return '';
}

// routeStripHTML lays out the strip's four beats: the origin, the stop the
// robot is at, the next, the final, and one +n for the stops between the next
// and the final, each a cell coloured and shaped by its state; the accent
// arrow before the stop the robot heads to; every stop named with its state
// in the title.
export function routeStripHTML(view, stops) {
  if (!view || !view.origin) return '-';
  var title = (stops || []).map(function (s) {
    var label = routeStateLabel(s.state);
    return (s.unassigned ? 'not assigned yet' : s.node) + (label ? ' (' + label + ')' : '');
  }).join(' → ');
  var cell = function (s) {
    if (s.unassigned) return '<span class="route-cell route-' + s.state + ' route-none" title="not assigned yet">—</span>';
    return '<span class="route-cell route-' + s.state + '">' + escapeHtml(s.node) + '</span>';
  };
  var chip = function (n, state, what) {
    return '<span class="route-cell route-chip route-' + state + '" title="' + n + ' ' + what + '">+' + n + '</span>';
  };
  var sep = '<span class="route-sep" aria-hidden="true">›</span>';
  var arrow = '<span class="route-sep route-heading" aria-hidden="true">›</span>';
  var before = function (beat) { return view.heading === beat ? arrow : sep; };
  var html = (view.heading === 'origin' ? arrow : '') + cell(view.origin);
  if (view.here) html += sep + cell(view.here);
  if (view.next) html += before('next') + cell(view.next);
  if (view.remaining) html += sep + chip(view.remaining, 'remaining', 'more');
  if (view.final) html += before('final') + cell(view.final);
  return '<span class="route-strip" title="' + escapeHtml(title) + '">' + html + '</span>';
}

// routeTimelineHTML lists every stop top to bottom with its state, the one
// place each stop of a long route is drawn. nodeHTML draws a node (the pop-up
// links it); notAssigned is what an end not chosen yet reads.
export function routeTimelineHTML(stops, nodeHTML, notAssigned) {
  if (!stops || !stops.length) return '';
  return '<ol class="route-timeline">' + stops.map(function (s) {
    var label = routeStateLabel(s.state);
    return '<li class="route-cell route-' + s.state + '"><span class="route-node">' +
      (s.unassigned ? notAssigned : nodeHTML(s.node)) + '</span>' +
      (label ? '<span class="route-label">' + label + '</span>' : '') + '</li>';
  }).join('') + '</ol>';
}
