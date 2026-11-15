/**
 * features/layout/usePointerFlare.ts -- the flare that follows the pointer.
 *
 * example.js:38-46 wrote `style.transform` on every mousemove, so the flare's
 * position lived in an inline style and the CSS could not own it. Here the
 * handler sets two custom properties and .admin-pointer-flare (index.css:314)
 * reads them, which keeps the geometry in the stylesheet.
 *
 * Two behaviours the template did not have:
 *  - the write is coalesced into one rAF per frame, so a fast drag does not
 *    queue dozens of style recalculations;
 *  - it does not attach at all under prefers-reduced-motion, which is what the
 *    UI suite runs with (testing/admin/playwright.config.ts:57). An element
 *    whose position depends on where a mouse happened to be is not screenshot
 *    stable.
 */

import { useEffect, useRef } from 'react';

export function usePointerFlare<T extends HTMLElement>(): React.RefObject<T | null> {
  const ref = useRef<T>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;

    let frame = 0;
    let x = 0;
    let y = 0;

    const paint = (): void => {
      frame = 0;
      el.style.setProperty('--admin-pointer-x', `${x}px`);
      el.style.setProperty('--admin-pointer-y', `${y}px`);
      el.style.opacity = '1';
    };

    const onMove = (e: MouseEvent): void => {
      x = e.clientX;
      y = e.clientY;
      if (!frame) frame = requestAnimationFrame(paint);
    };

    window.addEventListener('mousemove', onMove, { passive: true });
    return () => {
      window.removeEventListener('mousemove', onMove);
      if (frame) cancelAnimationFrame(frame);
    };
  }, []);

  return ref;
}
