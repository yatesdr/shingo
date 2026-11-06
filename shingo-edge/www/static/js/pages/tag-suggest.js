// tag-suggest.js — the tag combo's options for a counter, identity or station
// field, computed in the browser from WarLink's all-tags list. WarLink lists
// structs, not their members, and every production counter is a member: so a
// MES_* struct also offers its counter (or identity) member by the cell's
// convention, and a station struct (Sta010, Sta910_T1) its count member by
// the station family's. Each option says what is known of it — its type,
// whether another field is already configured on it, its live value when
// WarLink sent one, and whether it is published; a member offered by
// convention carries no value, since reading one would publish it. The typed
// value always wins: an option only saves keystrokes. A classic script, so
// its Node test loads it.
(function () {
    'use strict';

    const LEAF = {
        counter: { re: /\.Prod_Counter_\d+$/i, member: '.Prod_Counter_01' },
        identity: { re: /\.CATID_\d+$/i, member: '.CATID_01' },
    };

    function noteOf(t) {
        const parts = [];
        if (t.type) parts.push(t.type);
        if (t.configured) parts.push('configured');
        if (t.value !== undefined && t.value !== null && typeof t.value !== 'object') parts.push('reads ' + t.value);
        parts.push(t.enabled ? 'published' : 'NOT published');
        return parts.join(' · ');
    }

    // The station family: a station's struct, and the member that counts its
    // tool's cycles (one per load at a load station). A struct with an
    // operator-load tag beside it is where the part leaves the cart by hand.
    const STATION = /^Sta\d{2,3}(_\w+)?$/;
    const STATION_MEMBER = '.Prod_Tool_1_CNT.ACC';
    const CONVENTION = 'the family\u2019s convention \u2014 verified when saved';
    // NEVER_STATION: shapes that count something other than parts leaving a
    // cart — ware, guns and tips, timers, cycles, over-counts, indexes, FIFOs,
    // management totals, the marker's strings.
    const NEVER_STATION = /_Ware_Count|Gun|Tip_Change|_TMR\.|Cycle|Operator_Over_Count|Final_Index|_FIFO_CNTR|MGMT_|Telesis/i;

    // stationOptions: each station struct's count member, those with an
    // operator-load tag beside them first, then every other tag but the
    // shapes never offered, by name.
    function stationOptions(list) {
        const out = [];
        const seen = {};
        const add = (name, note) => {
            if (seen[name]) return;
            seen[name] = true;
            out.push({ name: name, note: note });
        };
        const listed = {};
        list.forEach(t => { listed[t.name] = t; });
        const stations = list.filter(t => STATION.test(t.name) && String(t.type || '').toUpperCase().indexOf('STRUCT') === 0);
        const loads = s => list.some(t => t.name.indexOf(s.name + '_PP_Op_Loaded_Parts') === 0);
        const member = (s, lead) => {
            const name = s.name + STATION_MEMBER;
            const known = listed[name] ? noteOf(listed[name]) : CONVENTION;
            add(name, lead ? 'operator load \u00b7 ' + known : known);
        };
        stations.filter(loads).forEach(s => member(s, true));
        stations.filter(s => !loads(s)).forEach(s => member(s, false));
        byName(list.filter(t => !NEVER_STATION.test(t.name) && String(t.type || '').toUpperCase() !== 'STRING'))
            .forEach(t => add(t.name, noteOf(t)));
        return out;
    }

    // byName is a copy of the tags in name order.
    function byName(list) {
        return list.slice().sort((a, b) => a.name.localeCompare(b.name));
    }

    // options(tags, kind): kind 'counter' or 'identity' puts the members of
    // that kind first — listed ones, then each MES_* struct's conventional
    // member — and every other tag after, by name; 'station' is
    // stationOptions; any other kind lists the tags by name. The combo keeps
    // this order among equally close matches.
    function options(tags, kind) {
        const list = Array.isArray(tags) ? tags.filter(t => t && t.name) : [];
        if (kind === 'station') return stationOptions(list);
        const leaf = LEAF[kind];
        if (!leaf) return byName(list).map(t => ({ name: t.name, note: noteOf(t) }));
        const out = [];
        const seen = {};
        const add = (name, note) => {
            if (seen[name]) return;
            seen[name] = true;
            out.push({ name: name, note: note });
        };
        list.filter(t => leaf.re.test(t.name)).forEach(t => add(t.name, noteOf(t)));
        list.filter(t => /^MES_/i.test(t.name) && t.name.indexOf('.') < 0)
            .forEach(t => add(t.name + leaf.member, 'member of ' + t.name + ' (by the cell convention)'));
        byName(list).forEach(t => add(t.name, noteOf(t)));
        return out;
    }

    // derivedIdentity is the identity tag the Edge derives from a counter tag
    // when none is typed: the counter's struct, member CATID_ with the
    // counter's index. "" when the counter names no struct member.
    function derivedIdentity(counterTag) {
        const t = String(counterTag || '').trim();
        const dot = t.lastIndexOf('.');
        if (dot <= 0) return '';
        const m = t.slice(dot + 1).match(/_(\d+)$/);
        return t.slice(0, dot) + '.CATID_' + (m ? m[1] : '01');
    }

    const api = { options, derivedIdentity, noteOf };
    if (typeof module !== 'undefined' && module.exports) module.exports = api;
    if (typeof window !== 'undefined') window.TagSuggest = api;
})();
