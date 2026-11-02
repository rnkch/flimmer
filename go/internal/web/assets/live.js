// Live updates reach the page over the live connection: the server pushes the playlist, its
// summary and the player card (handlers.go, events). While the list is frozen for a drag
// (hx-morph-skip) a push does nothing; this asks for the current list afterwards.
export function refreshPlaylist() {
  htmx.ajax('GET', '/playlist/list', { target: '#playlist', swap: 'outerMorph' });
}
