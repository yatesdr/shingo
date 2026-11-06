// tag-suggest.js — the tag combo's options for a counter or identity field,
// computed in the browser from WarLink's all-tags list. WarLink lists structs,
// not their members, and every production counter is a member: so a MES_*
// struct also offers its counter (or identity) member by the cell's
// convention. Each option says what is known of it — its type, whether
// another field is already configured on it, its live value when WarLink sent
// one, and whether it is published. The typed value always wins: an option
// only saves keystrokes. A classic script, so its Node test loads it.
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

    // options(tags, kind): kind 'counter' or 'identity' puts the members of
    // that kind first — listed ones, then each MES_* struct's conventional
    // member — and every other tag after; any other kind lists the tags as
    // they come.
    function options(tags, kind) {
        const list = Array.isArray(tags) ? tags.filter(t => t && t.name) : [];
        const leaf = LEAF[kind];
        if (!leaf) return list.map(t => ({ name: t.name, note: noteOf(t) }));
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
        list.forEach(t => add(t.name, noteOf(t)));
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
