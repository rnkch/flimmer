import { files as nFiles, t } from './i18n.js';

// Drop files from the desktop onto the playlist: they are uploaded and
// inserted where they were dropped. Row reordering (sortable.js) uses the
// same drag events but carries no files, so the two don't collide.
export function initDropUpload() {
  let marked = null;

  const hasFiles = (e) => [...(e.dataTransfer?.types ?? [])].includes('Files');
  const zoneOf = (e) => e.target.closest?.('[data-dropzone]');

  function mark(row, after) {
    marked?.classList.remove('drop-before', 'drop-after');
    marked = row;
    row?.classList.add(after ? 'drop-after' : 'drop-before');
  }

  // The list is frozen for live updates (hx-morph-skip) while files hover over it: a morph
  // would remove the drop markers. unmark() lifts it and asks for the update it skipped.
  function unmark() {
    mark(null);
    document.querySelectorAll('[data-dropzone].dropping').forEach((z) => z.classList.remove('dropping'));
    document.querySelectorAll('[data-sortable][hx-morph-skip]').forEach((list) => {
      list.removeAttribute('hx-morph-skip');
      htmx.trigger(document.body, 'playlist');
    });
  }

  // 1-based playlist position under the pointer; 0 = append.
  function positionAt(zone, e) {
    const rows = [...zone.querySelectorAll('[data-sortable] > [data-id]')];
    for (const [i, row] of rows.entries()) {
      const box = row.getBoundingClientRect();
      if (e.clientY < box.top + box.height / 2) {
        mark(row, false);
        return i + 1;
      }
    }
    if (rows.length) mark(rows[rows.length - 1], true);
    return 0;
  }

  document.addEventListener('dragover', (e) => {
    const zone = zoneOf(e);
    if (!zone || !hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
    zone.classList.add('dropping');
    zone.querySelector('[data-sortable]')?.setAttribute('hx-morph-skip', '');
    positionAt(zone, e);
  });

  document.addEventListener('dragleave', (e) => {
    if (!e.relatedTarget || !zoneOf({ target: e.relatedTarget })) unmark();
  });

  document.addEventListener('drop', (e) => {
    const zone = zoneOf(e);
    if (!zone || !hasFiles(e)) return;
    e.preventDefault();
    const position = positionAt(zone, e);
    unmark();
    upload(zone, e.dataTransfer.files, position);
  });
}

function upload(zone, files, position) {
  const form = new FormData();
  for (const f of files) form.append('files', f);
  if (position > 0) form.append('position', String(position));

  const bar = zone.querySelector('.drop-progress');
  bar.hidden = false;
  bar.value = 0;
  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/upload');
  xhr.setRequestHeader('HX-Request', 'true');
  xhr.upload.onprogress = (e) => {
    if (e.lengthComputable) bar.value = Math.round((e.loaded / e.total) * 100);
  };
  xhr.onload = () => {
    bar.hidden = true;
    const ok = xhr.status >= 200 && xhr.status < 300;
    const n = files.length === 1 ? files[0].name : nFiles(files.length);
    toast(ok ? t('{0} uploaded, processing', n) : xhr.responseText || t('Upload failed (HTTP {0})', xhr.status), ok ? 'success' : 'danger');
    if (ok) htmx.trigger(document.body, 'jobsChanged');
  };
  xhr.onerror = () => {
    bar.hidden = true;
    toast(t('Connection lost'), 'danger');
  };
  xhr.send(form);
}

function toast(message, variant) {
  document.dispatchEvent(new CustomEvent('showToast', { detail: { message, variant } }));
}
