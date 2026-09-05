/**
 * Turns vertical touch drags into wheel ticks for programs that track the mouse.
 *
 * With tmux (`mouse on`), vim or less in the foreground, the scrollback lives in
 * that program, not in xterm.js. Desktop mouse wheels reach it because xterm
 * encodes wheel events as mouse reports, but a touch drag only moves xterm's own
 * (empty) viewport. The tracker below measures a drag in screen rows and emits
 * one wheel tick per `tickPixels`, so tmux's default five-line wheel step moves
 * the content as far as the finger did.
 */

/** tmux's default copy-mode step for WheelUpPane / WheelDownPane. */
export const TMUX_WHEEL_LINES = 5;
/** Movement below this is a tap or jitter, not a scroll. */
const AXIS_SLOP_PX = 8;
/** Momentum decays with this time constant; ~1.2 s to come to rest from a brisk swipe. */
const FLING_DECAY_MS = 320;
/** Velocities below this (px/ms) end a fling; above this a release starts one. */
const FLING_STOP_SPEED = 0.02;
const FLING_START_SPEED = 0.25;

export type TouchScrollAxis = 'undecided' | 'vertical' | 'horizontal';

export class TouchScrollTracker {
  axis: TouchScrollAxis = 'undecided';
  private startX = 0;
  private startY = 0;
  private lastY = 0;
  private lastTime = 0;
  private carry = 0;
  /** Finger speed in px/ms; positive means the finger moves down, i.e. older lines scroll into view. */
  private velocity = 0;
  private coasting = false;

  constructor(readonly tickPixels: number) {
    if (!(tickPixels > 0)) throw new RangeError('tickPixels must be positive');
  }

  begin(x: number, y: number, time: number): void {
    this.axis = 'undecided';
    this.startX = x;
    this.startY = y;
    this.lastY = y;
    this.lastTime = time;
    this.carry = 0;
    this.velocity = 0;
    this.coasting = false;
  }

  /**
   * Feeds a finger position. Returns null while the gesture has not committed to
   * the vertical axis (so the browser may still pan sideways), otherwise the
   * wheel ticks owed so far: positive ticks scroll up towards older lines.
   */
  move(x: number, y: number, time: number): number | null {
    if (this.axis === 'undecided') {
      const dx = Math.abs(x - this.startX);
      const dy = Math.abs(y - this.startY);
      if (Math.max(dx, dy) < AXIS_SLOP_PX) return null;
      this.axis = dy >= dx ? 'vertical' : 'horizontal';
      this.lastY = y;
      this.lastTime = time;
      return this.axis === 'vertical' ? 0 : null;
    }
    if (this.axis === 'horizontal') return null;
    const distance = y - this.lastY;
    const elapsed = time - this.lastTime;
    if (elapsed > 0) {
      // Exponential smoothing keeps a single jittery sample from dominating the fling.
      const instant = distance / elapsed;
      this.velocity = this.velocity === 0 ? instant : this.velocity * 0.6 + instant * 0.4;
    }
    this.lastY = y;
    this.lastTime = time;
    return this.consume(distance);
  }

  /** Finger lifted: keeps the last velocity for coasting when the swipe was brisk. */
  release(): void {
    this.coasting = this.axis === 'vertical' && Math.abs(this.velocity) >= FLING_START_SPEED;
    if (!this.coasting) this.velocity = 0;
  }

  /** True while a fling still has momentum; advance it with coast(). */
  get isCoasting(): boolean {
    return this.coasting;
  }

  /** Advances a fling by `elapsed` ms and returns the ticks owed for that interval. */
  coast(elapsed: number): number {
    if (!this.coasting || elapsed <= 0) return 0;
    const decay = Math.exp(-elapsed / FLING_DECAY_MS);
    // Distance travelled by an exponentially decaying velocity over the interval.
    const distance = this.velocity * FLING_DECAY_MS * (1 - decay);
    this.velocity *= decay;
    if (Math.abs(this.velocity) < FLING_STOP_SPEED) {
      this.velocity = 0;
      this.coasting = false;
    }
    return this.consume(distance);
  }

  /** Stops any fling, e.g. because a new touch started. */
  stop(): void {
    this.velocity = 0;
    this.coasting = false;
  }

  private consume(distance: number): number {
    this.carry += distance;
    const ticks = Math.trunc(this.carry / this.tickPixels);
    this.carry -= ticks * this.tickPixels;
    return ticks;
  }
}

/**
 * Dispatches `ticks` synthetic wheel events at the given viewport position.
 * xterm's mouse-tracking listener turns each one into a wheel report for the
 * program; negative deltaY is wheel-up, which programs treat as "older lines".
 */
export function dispatchWheelTicks(target: EventTarget, ticks: number, clientX: number, clientY: number): void {
  const deltaY = ticks > 0 ? -1 : 1;
  for (let remaining = Math.abs(ticks); remaining > 0; remaining -= 1) {
    target.dispatchEvent(
      new WheelEvent('wheel', {
        deltaY,
        deltaMode: WheelEvent.DOM_DELTA_LINE,
        clientX,
        clientY,
        bubbles: true,
        cancelable: true,
      }),
    );
  }
}
