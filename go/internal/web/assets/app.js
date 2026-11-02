// Browser entry point. Bundled at startup by internal/web/bundle.
// Order matters: htmx must be on window before its extensions run.
import 'htmx.org';
import 'htmx.org/ext/hx-sse.js';
import 'htmx.org/ext/hx-alpine-compat.js';
import Alpine from 'alpinejs';

import '@awesome.me/webawesome/components/badge/badge.js';
import '@awesome.me/webawesome/components/button/button.js';
import '@awesome.me/webawesome/components/button-group/button-group.js';
import '@awesome.me/webawesome/components/callout/callout.js';
import '@awesome.me/webawesome/components/card/card.js';
import '@awesome.me/webawesome/components/dialog/dialog.js';
import '@awesome.me/webawesome/components/divider/divider.js';
import '@awesome.me/webawesome/components/dropdown/dropdown.js';
import '@awesome.me/webawesome/components/dropdown-item/dropdown-item.js';
import '@awesome.me/webawesome/components/format-bytes/format-bytes.js';
import '@awesome.me/webawesome/components/icon/icon.js';
import '@awesome.me/webawesome/components/input/input.js';
import '@awesome.me/webawesome/components/number-input/number-input.js';
import '@awesome.me/webawesome/components/option/option.js';
import '@awesome.me/webawesome/components/popover/popover.js';
import '@awesome.me/webawesome/components/progress-bar/progress-bar.js';
import '@awesome.me/webawesome/components/select/select.js';
import '@awesome.me/webawesome/components/spinner/spinner.js';
import '@awesome.me/webawesome/components/switch/switch.js';
import '@awesome.me/webawesome/components/tag/tag.js';
import '@awesome.me/webawesome/components/textarea/textarea.js';
import '@awesome.me/webawesome/components/toast/toast.js';
import '@awesome.me/webawesome/components/toast-item/toast-item.js';
import '@awesome.me/webawesome/components/tooltip/tooltip.js';
// Web Awesome's own strings (close buttons, etc.) and number formats follow <html lang>.
import '@awesome.me/webawesome/translations/de.js';
import { registerIconLibrary } from '@awesome.me/webawesome/components/icon/library.js';

import { initSortable } from './sortable.js';
import { initDropUpload } from './drop.js';
import { uploader } from './upload.js';
import { seekBar } from './seek.js';

// Icons are served from the binary instead of the Font Awesome CDN.
registerIconLibrary('default', { resolver: (name) => `/static/icons/${name}.svg` });

window.Alpine = Alpine;
Alpine.data('uploader', uploader);
Alpine.data('seekBar', seekBar);
Alpine.start();

initSortable();
initDropUpload();

// Inline editors arrive with [autofocus]; focus them after the swap.
htmx.onLoad((elt) => elt.querySelector?.('[autofocus]')?.focus());

// The server restarts itself (an upgrade, "Restart service", /upgrade or /restart in
// Telegram) or crashes, and the page is stale then: a new version, maybe new asset names.
// Two ways to notice, both ending in one reload:
//
// 1. Every live connection (/events, see the hx-sse element in the layout) starts with a
//    "boot" event carrying a random id that is new on every start of the server. The
//    connection drops while the server is down and htmx reconnects by itself (0.5 s,
//    doubling, up to a minute). If the id after a reconnect isn't the one this page
//    began with, the server has restarted. This reaches every open tab, whoever
//    started the restart.
// 2. The tab that asked for the restart also gets HX-Trigger "serverRestarting" and polls
//    /login (every 1.5 s, X-Boot header) so it doesn't wait for the reconnect backoff.
//    It gives up after two minutes and reloads anyway.
let reloading = false;
const reloadOnce = () => {
  if (!reloading) {
    reloading = true;
    location.reload();
  }
};

let bootId = null;
document.addEventListener('boot', (e) => {
  const id = e.detail.data;
  if (bootId !== null && id !== bootId) reloadOnce();
  bootId ??= id;
});

document.addEventListener('serverRestarting', async () => {
  const currentBootId = async () => {
    try {
      return (await fetch('/login', { cache: 'no-store' })).headers.get('X-Boot');
    } catch {
      return null; // the server is down
    }
  };
  const before = await currentBootId();
  const until = Date.now() + 120_000;
  while (Date.now() < until && !reloading) {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    const now = await currentBootId();
    if (now && now !== before) break;
  }
  reloadOnce();
});

// Notifications. The server sends HX-Trigger: {"showToast": {"message", "variant"}};
// client code dispatches the same event.
const icons = { success: 'circle-check', danger: 'triangle-exclamation', warning: 'circle-exclamation' };
document.addEventListener('showToast', (e) => {
  const { message, variant = 'neutral' } = e.detail;
  document.querySelector('wa-toast')?.create(message, {
    variant,
    icon: icons[variant] ?? 'circle-info',
    duration: variant === 'danger' ? 8000 : 4000,
  });
});
