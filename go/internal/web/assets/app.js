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

// After the service restarts itself (an upgrade, "Restart service") the server sends
// HX-Trigger "serverRestarting". The page is stale then (new version, maybe new assets),
// so wait until the server answers again with another boot id (X-Boot) and reload. A
// server that is down gives no id; whatever answers next counts as the new one. Gives up
// waiting after two minutes and reloads anyway.
document.addEventListener('serverRestarting', async () => {
  const bootId = async () => {
    try {
      return (await fetch('/login', { cache: 'no-store' })).headers.get('X-Boot');
    } catch {
      return null;
    }
  };
  const before = await bootId();
  const until = Date.now() + 120_000;
  while (Date.now() < until) {
    await new Promise((resolve) => setTimeout(resolve, 1500));
    const now = await bootId();
    if (now && now !== before) break;
  }
  location.reload();
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
