// @vitest-environment jsdom

import { describe, expect, it, vi } from 'vitest';
import { dispatchWheelTicks, TouchScrollTracker } from './touchScroll';

describe('TouchScrollTracker', () => {
  it('waits for the gesture to commit to an axis before producing ticks', () => {
    const tracker = new TouchScrollTracker(50);
    tracker.begin(100, 100, 0);
    expect(tracker.move(103, 104, 10)).toBeNull();
    expect(tracker.axis).toBe('undecided');
    expect(tracker.move(120, 102, 20)).toBeNull();
    expect(tracker.axis).toBe('horizontal');
    expect(tracker.move(200, 300, 30)).toBeNull();
  });

  it('converts vertical travel into whole ticks and carries the remainder', () => {
    const tracker = new TouchScrollTracker(50);
    tracker.begin(100, 100, 0);
    expect(tracker.move(100, 110, 10)).toBe(0);
    expect(tracker.axis).toBe('vertical');
    // Finger moving down (older lines) yields positive ticks.
    expect(tracker.move(100, 180, 20)).toBe(1);
    expect(tracker.move(100, 205, 30)).toBe(0);
    expect(tracker.move(100, 235, 40)).toBe(1);
    // Reversing direction yields negative ticks once the carry is overcome:
    // -115px on a +25px carry is -90px, i.e. one tick down with -40px left over.
    expect(tracker.move(100, 120, 50)).toBe(-1);
    expect(tracker.move(100, 100, 60)).toBe(-1);
  });

  it('coasts after a brisk release and stops on its own', () => {
    const tracker = new TouchScrollTracker(20);
    tracker.begin(0, 0, 0);
    tracker.move(0, 10, 10);
    tracker.move(0, 40, 20);
    tracker.move(0, 70, 30);
    tracker.release();
    expect(tracker.isCoasting).toBe(true);
    let ticks = 0;
    for (let frame = 0; frame < 200 && tracker.isCoasting; frame += 1) ticks += tracker.coast(16);
    expect(ticks).toBeGreaterThan(0);
    expect(tracker.isCoasting).toBe(false);
    expect(tracker.coast(16)).toBe(0);
  });

  it('does not coast after a slow release or a horizontal gesture', () => {
    const slow = new TouchScrollTracker(20);
    slow.begin(0, 0, 0);
    slow.move(0, 10, 100);
    slow.move(0, 12, 200);
    slow.release();
    expect(slow.isCoasting).toBe(false);

    const sideways = new TouchScrollTracker(20);
    sideways.begin(0, 0, 0);
    sideways.move(40, 2, 10);
    sideways.release();
    expect(sideways.isCoasting).toBe(false);
  });
});

describe('dispatchWheelTicks', () => {
  it('emits one wheel event per tick with wheel-up for positive ticks', () => {
    const target = document.createElement('div');
    const seen: number[] = [];
    target.addEventListener('wheel', (event) => seen.push(event.deltaY));
    dispatchWheelTicks(target, 2, 10, 20);
    dispatchWheelTicks(target, -1, 10, 20);
    expect(seen).toEqual([-1, -1, 1]);
  });

  it('carries the pointer position so xterm can report a cell', () => {
    const target = document.createElement('div');
    const listener = vi.fn();
    target.addEventListener('wheel', listener);
    dispatchWheelTicks(target, 1, 33, 44);
    const event = listener.mock.calls[0]?.[0] as WheelEvent;
    expect(event.clientX).toBe(33);
    expect(event.clientY).toBe(44);
    expect(event.cancelable).toBe(true);
  });
});
