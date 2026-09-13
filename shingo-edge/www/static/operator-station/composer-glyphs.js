// composer-glyphs.js — swap-mode glyphs for the Flow Composer (station HMI). Slots, bins and streams, 2026-09-10.
// Design sheet: hmi-flow-composer-design-2026-09-02/glyphs/swap-mode-glyphs.html · styles: composer-glyphs.css
// glyph(mode, size, {rest}) -> one inline <svg viewBox="0 0 28 28">
const svg=(g,size,rest)=>`<svg class="gl${rest?' rest':''}" viewBox="0 0 28 28" width="${size}" height="${size}" aria-hidden="true">${g}</svg>`;
const ang=(a,b)=>Math.atan2(b[1]-a[1],b[0]-a[0])*180/Math.PI;
function glyph(mode,size,o){o=o||{};
  // One grid for every glyph: slot 6.6, bin 4, and a constant gap G between any two things. A chevron pair
  // is centred in its gap, so the clearance on both sides of every pair is identical.
  const G=6, S=6.6, B=4, hS=S/2, hB=B/2;
  const press=(x1,x2)=>`<path class="press" d="M${x1} 2.6H${x2}"/>`;
  const slot=(x,y)=>`<rect class="slot" x="${(x-hS).toFixed(2)}" y="${(y-hS).toFixed(2)}" width="${S}" height="${S}" rx="1.9"/>`;
  const bin=(x,y,old)=>`<rect class="bin${old?' old':''}" x="${(x-hB).toFixed(2)}" y="${(y-hB).toFixed(2)}" width="${B}" height="${B}" rx="1"/>`;
  // one move = one pair of slender chevrons, centred on the gap a→b (a and b are the two edges it sits between).
  const move=(a,b,r)=>{const dx=b[0]-a[0],dy=b[1]-a[1],L=Math.hypot(dx,dy),ux=dx/L,uy=dy/L,th=ang(a,b);
    const mx=(a[0]+b[0])/2+ux*0.75,my=(a[1]+b[1])/2+uy*0.75;   // visual span runs from tail(-2.5) to tip(+1): centre that, not the tips
    return [-1,1].map(d=>`<path class="sc ${r}" d="M-1.5 -1.1L0 0L-1.5 1.1" transform="translate(${(mx+ux*d).toFixed(2)} ${(my+uy*d).toFixed(2)}) rotate(${th.toFixed(1)})"/>`).join('');};
  let g='';
  if(mode==='two_robot_press_index'){
    // press slot, on-deck slot G below it; a bin G to the left of the on-deck slot, a bin G to the right of the press slot
    const T=9.5, D=T+hS+G+hS;                       // 9.5, 22.1
    g+=press(7,21)+slot(14,T)+slot(14,D)+bin(14,D,false);
    g+=bin(14-hS-G-hB,D,false)+move([14-hS-G,D],[14-hS,D],'r1');      // Robot 1: next tote in, behind the one on deck
    g+=move([14,D-hS],[14,T+hS],'r2');                                  // Robot 2: index up into the press
    g+=move([14+hS,T],[14+hS+G,T],'r2')+bin(14+hS+G+hB,T,true);        // Robot 2: old tote out
  } else if(mode==='two_robot'){
    // one press slot; the new bin comes straight up into it, the old bin goes straight out to the right
    const T=9.5, X=14-(G+B)/2, Yb=T+hS+G+hB;                              // slot at 9 so slot+gap+bin centres on 14
    g+=press(X-7,X+7)+slot(X,T);
    g+=bin(X,Yb,false)+move([X,T+hS+G],[X,T+hS],'r1');                    // Robot 1: new bin in
    g+=move([X+hS,T],[X+hS+G,T],'r2')+bin(X+hS+G+hB,T,true);             // Robot 2: old bin out
  } else if(mode==='single_robot'){
    // three slots on an equilateral triangle, centre-to-centre S+G on every side, so all three gaps match
    const T=9.5, d=S+G, Lx=14-d/2, Rx=14+d/2, Y=T+d*0.866;
    const c=3.74;                                                        // where a 60° diagonal leaves a slot's rounded corner
    g+=press(7,21)+slot(14,T)+bin(14,T,true)+slot(Lx,Y)+bin(Lx,Y,false)+slot(Rx,Y);
    g+=move([Lx+0.5*c,Y-0.866*c],[14-0.5*c,T+0.866*c],'r1');            // park → press: new bin in
    g+=move([14+0.5*c,T+0.866*c],[Rx-0.5*c,Y-0.866*c],'r1');            // press → other park: old bin out
    g+=move([Rx-hS,Y],[Lx+hS,Y],'r1');                                   // back across for the next one
  } else if(mode==='sequential'){
    // A holds the bin the line is pulling from; B is where Robot 1 works — empty out to the right, new bin up from below
    const T=9.5, Bx=13.25, Ax=Bx-hS-G-hS, Yb=T+hS+G+hB;                   // 4.75, 13.25, 20.8 — whole row centres on 14
    g+=press(Ax-hS+0.3,Bx+hS-0.3)+slot(Ax,T)+bin(Ax,T,false)+slot(Bx,T);
    g+=bin(Bx,Yb,false)+move([Bx,T+hS+G],[Bx,T+hS],'r1');                 // Robot 1: new bin up into B
    g+=move([Bx+hS,T],[Bx+hS+G,T],'r1')+bin(Bx+hS+G+hB,T,true);          // Robot 1: empty out of B
  } else { g+=`<rect x="6" y="6" width="16" height="16" rx="3.5" fill="none" stroke="var(--sub-3)" stroke-width="2.4" stroke-dasharray="3 2.5"/>`; }
  return svg(g,size,o.rest);
}
if (typeof module !== 'undefined') module.exports = { glyph };
