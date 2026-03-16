// shapes.js — Shape creation, cloning, serialization for operator canvas

import { DEFAULT_SIZES, CANVAS_W, CANVAS_H, applyShapeProxy } from './render.js';

function uuid() {
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
        const r = Math.random() * 16 | 0;
        return (c === 'x' ? r : (r & 0x3 | 0x8)).toString(16);
    });
}

export function createShape(type, x, y, opts = {}) {
    const size = DEFAULT_SIZES[type] || { w: 200, h: 120 };
    const id = uuid();
    let config;

    switch (type) {
        case 'ordercombo':
            config = {
                x, y, w: size.w, h: size.h,
                payloadId: opts.payloadId || null,
                payloadCode: opts.payloadCode || '',
                description: opts.description || 'Payload',
                lineId: opts.lineId || null,
                payloadStatus: 'active',
                remainingPct: 100, remaining: 0, total: 0,
                orderStatus: '', orderETA: '',
                actionLabel: opts.actionLabel || 'REQUEST',
                actionType: opts.actionType || 'retrieve',
                retrieveEmpty: opts.retrieveEmpty != null ? opts.retrieveEmpty : true,
                backgroundColor: '#1E1E1E',
            };
            break;
        case 'header':
            config = { x: 0, y: 0, w: CANVAS_W, h: size.h, text: opts.text || 'Operator Station', textX: 0.5, textY: 0.5 };
            break;
        case 'statusbar':
            config = { x: 0, y: CANVAS_H - size.h, w: CANVAS_W, h: size.h, lineName: opts.lineName || 'Line', lineId: opts.lineId || null, styleName: '' };
            break;
        case 'label':
            config = { x, y, w: size.w, h: size.h, text: opts.text || 'Label', textColor: '#FFFFFF', fontSize: 32, textAlign: 'center' };
            break;
        default:
            config = { x, y, w: size.w, h: size.h };
    }

    return applyShapeProxy({ id, type, config });
}

export function cloneShape(original) {
    const cloned = { id: uuid(), type: original.type, config: JSON.parse(JSON.stringify(original.config)) };
    cloned.config.x += 20;
    cloned.config.y += 20;
    return applyShapeProxy(cloned);
}

export function serializeShapes(shapes) {
    return shapes.map(s => ({ id: s.id, type: s.type, config: { ...s.config, _btnBounds: undefined } }));
}

export function hydrateShapes(arr) {
    if (!Array.isArray(arr)) return [];
    return arr.map(s => applyShapeProxy({ id: s.id, type: s.type, config: { ...s.config } }));
}
