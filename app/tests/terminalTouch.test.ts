import assert from "node:assert/strict";
import { test } from "node:test";
import { createTouchScroll } from "../src/terminalTouch.ts";

function fixture() {
  let time = 0, id = 0, momentum = true, canScroll = true;
  const frames = new Map<number, FrameRequestCallback>();
  const travel: number[] = [];
  const touch = createTouchScroll(pixels => { travel.push(pixels); return canScroll; }, {
    now: () => time, momentum: () => momentum,
    schedule: callback => { frames.set(++id, callback); return id; },
    cancel: frame => { frames.delete(frame); },
  });
  const tick = (elapsed = 16) => {
    time += elapsed;
    const callbacks = [...frames.values()]; frames.clear();
    callbacks.forEach(callback => callback(time));
  };
  const move = (y: number, elapsed = 16, x = 0) => { time += elapsed; return touch.move({ x, y }); };
  const flick = () => {
    touch.start({ x: 0, y: 500 });
    for (const y of [470, 440, 410, 380]) { move(y); tick(0); }
    touch.end();
  };
  return { touch, tick, move, flick, travel, frames,
    total: () => travel.reduce((sum, pixels) => sum + pixels, 0),
    pause: (elapsed: number) => { time += elapsed; },
    reduce: () => { momentum = false; }, edge: () => { canScroll = false; },
  };
}

test("small finger movement is retained and multiple events share one display update", () => {
  const f = fixture(); f.touch.start({ x: 0, y: 100 });
  assert(f.move(93.5)); assert(f.move(93.25));
  assert.equal(f.frames.size, 1); assert.equal(f.total(), 0);
  f.tick(); assert.equal(f.total(), 6.75);
  assert.equal(f.frames.size, 0, "holding a finger must not coast");
  f.pause(120); f.touch.end(); f.tick();
  assert.equal(f.total(), 6.75, "lifting after a pause must not restart scrolling");
});

test("flicks decelerate after release and are independent of display refresh rate", () => {
  const coast = (hz: number) => {
    const f = fixture(); f.flick(); const dragged = f.total();
    for (let i = 0; f.frames.size && i < hz * 2; i++) f.tick(1000 / hz);
    assert.equal(f.frames.size, 0, "momentum must finish promptly");
    assert(f.total() > dragged + 100);
    assert(f.total() < dragged + 700, "limit velocity from unusually fast event bursts");
    const tail = f.travel.slice(4);
    assert(tail.every((pixels, i) => pixels > 0 && (!i || pixels <= tail[i - 1])));
    return f.total();
  };
  assert(Math.abs(coast(60) - coast(120)) < 1);
});

test("new touches stop a fling immediately and direction reversals follow the finger", () => {
  const f = fixture(); f.flick(); f.tick();
  f.touch.start({ x: 0, y: 100 });
  const stopped = f.total(); f.tick(); assert.equal(f.total(), stopped);
  f.move(120); f.tick(0); assert(f.travel.at(-1)! < 0);
  f.touch.stop(); f.tick(); assert.equal(f.frames.size, 0);
});

test("edges, reduced motion, suspended frames and disposal stop momentum", () => {
  for (const action of ["edge", "reduce", "stall", "dispose"] as const) {
    const f = fixture(); f.flick();
    if (action === "edge") f.edge();
    if (action === "reduce") f.reduce();
    if (action === "dispose") f.touch.dispose();
    f.tick(action === "stall" ? 200 : 16);
    assert.equal(f.frames.size, 0, action);
    if (action === "dispose") { f.touch.start({ x: 0, y: 0 }); f.move(100); assert.equal(f.frames.size, 0); }
  }
  const f = fixture(); f.reduce(); f.flick();
  assert.equal(f.frames.size, 0, "reduced motion must retain direct dragging without a release animation");
  assert.equal(f.total(), 120);
});

test("taps and horizontal gestures do not claim terminal scrolling", () => {
  const f = fixture(); f.touch.start({ x: 0, y: 100 });
  assert.equal(f.move(97), false);
  assert.equal(f.move(95, 16, 30), false);
  assert.equal(f.move(0), false);
  f.touch.end(); assert.equal(f.frames.size, 0); assert.equal(f.total(), 0);
});
