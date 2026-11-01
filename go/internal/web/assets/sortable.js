// Drag and drop reordering for the playlist with native HTML drag events.
// On drop it posts the new 1-based position; the server answers with the
// updated list, which htmx swaps in.
export function initSortable() {
  let dragged = null;

  document.addEventListener('dragstart', (e) => {
    const row = e.target.closest?.('[data-sortable] > [data-id][draggable=true]');
    if (!row) return;
    dragged = row;
    row.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
  });

  document.addEventListener('dragover', (e) => {
    if (!dragged) return;
    const row = e.target.closest?.('[data-sortable] > [data-id]');
    if (!row || row === dragged || row.parentElement !== dragged.parentElement) return;
    e.preventDefault();
    const box = row.getBoundingClientRect();
    const after = e.clientY > box.top + box.height / 2;
    row.parentElement.insertBefore(dragged, after ? row.nextSibling : row);
  });

  document.addEventListener('dragend', () => {
    if (!dragged) return;
    const row = dragged;
    dragged = null;
    row.classList.remove('dragging');
    const list = row.parentElement;
    const pos = [...list.children].indexOf(row) + 1;
    if (String(pos) === row.dataset.position) return;
    htmx.ajax('POST', `/media/${row.dataset.id}/move`, {
      target: list, swap: 'outerMorph', values: { position: pos },
    });
  });
}
