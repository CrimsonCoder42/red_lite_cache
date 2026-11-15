/**
 * features/layout/AppShell.tsx -- sidebar + overlay + content.
 *
 * Geometry is the template's: example.html:29 (the flex row), :31 (w-64
 * sidebar), :114 (the mobile overlay), :116 (main-content with lg:ml-64 applied
 * by example.js:72), :134 (main, p-6 / lg:p-8, max-w-[95rem] centred at :135).
 *
 * The sidebar starts CLOSED below lg and OPEN from lg up. The template shipped
 * it closed at every width (`-translate-x-full` at example.html:31 with nothing
 * opening it on load), so a desktop visitor arrived at a hidden nav and had to
 * find the hamburger.
 *
 * Renders <Outlet />, so the routes it wraps supply the page. That is what lets
 * the new shell host the fork's existing pages unchanged while they are replaced
 * one at a time.
 */

import React, { useCallback, useEffect, useState } from 'react';
import { Outlet, useLocation } from 'react-router-dom';
import { NewModalProvider } from '@/shared/modals';
import GlassSidebar from './GlassSidebar';
import Topbar from './Topbar';
import { navTitle } from './adminNav';
import { usePointerFlare } from './usePointerFlare';

/** Tailwind's lg. The one place the breakpoint is a number. */
const LG = 1024;

const isWide = (): boolean => typeof window === 'undefined' || window.innerWidth >= LG;

export const AppShell: React.FC = () => {
  const [sidebarOpen, setSidebarOpen] = useState<boolean>(isWide);
  const { pathname } = useLocation();
  const flareRef = usePointerFlare<HTMLDivElement>();

  /* Crossing the breakpoint re-decides the default. Resizing from a narrow
     window to a wide one otherwise leaves a desktop viewport with no nav. */
  useEffect(() => {
    const onResize = (): void => setSidebarOpen(isWide());
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);

  /* example.js:143-145 -- a narrow viewport closes the drawer after a tap. */
  const onNavigate = useCallback(() => {
    if (!isWide()) setSidebarOpen(false);
  }, []);

  /**
   * NewModalProvider is mounted HERE, not in main.tsx, because it is the shell
   * that replaced the only thing that used to mount it
   * (layout/UnifiedLayout.tsx:26). Without this line every legacy page calling
   * openModal throws "must be used within a NewModalProvider" the moment its
   * shell changed -- a regression the swap would otherwise have introduced.
   *
   * ONE modal system: features/ui/Modal.tsx is a skin over this provider's
   * BaseModal, not a second portal (D-073).
   *
   * Not carried over from UnifiedLayout: PageRefreshOverlay, and two <AdSlot>
   * instances (UnifiedLayout.tsx:40-42). A staff console does not serve ads.
   */
  return (
    <NewModalProvider>
    <div className="admin-shell flex min-h-screen">
      {/* example.html:25-27 */}
      <div className="admin-flare admin-flare--a" aria-hidden="true" />
      <div className="admin-flare admin-flare--b" aria-hidden="true" />
      <div ref={flareRef} className="admin-pointer-flare opacity-0" aria-hidden="true" />

      <GlassSidebar open={sidebarOpen} onNavigate={onNavigate} />

      {/* example.html:114 -- dismisses the drawer, and only exists below lg. */}
      {sidebarOpen && (
        <button
          type="button"
          aria-label="Close navigation"
          onClick={() => setSidebarOpen(false)}
          className="fixed inset-0 z-30 bg-black/50 lg:hidden"
        />
      )}

      <div
        className={[
          'flex min-w-0 flex-1 flex-col transition-all duration-300 ease-in-out',
          sidebarOpen ? 'lg:ml-64' : '',
        ].join(' ')}
      >
        <Topbar
          onToggleSidebar={() => setSidebarOpen((v) => !v)}
          sidebarOpen={sidebarOpen}
          title={navTitle(pathname)}
        />
        <main className="flex-1 overflow-y-auto p-6 lg:p-8">
          <div className="mx-auto max-w-[95rem]">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
    </NewModalProvider>
  );
};

export default AppShell;
