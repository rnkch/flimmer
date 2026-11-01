// Click-to-seek on the player's progress bar (Player card only). The click
// position becomes a time in seconds; htmx posts it (hx-trigger="seek" on the
// bar). While hovering, the target time is shown between the two times.
export function seekBar() {
  return {
    hover: '',

    // at returns the time under the pointer, from the bar's own width.
    at(e) {
      const r = e.currentTarget.getBoundingClientRect();
      const f = Math.min(Math.max((e.clientX - r.left) / r.width, 0), 1);
      return f * Number(this.$root.dataset.duration);
    },

    jump(e) {
      this.$refs.pos.value = this.at(e).toFixed(1);
      e.currentTarget.dispatchEvent(new CustomEvent('seek', { bubbles: true }));
    },

    preview(e) {
      this.hover = '→ ' + clock(this.at(e));
    },
  };
}

// clock formats seconds like the server's views.Dur: "1:23:45" or "3:07".
function clock(sec) {
  const s = Math.floor(sec);
  const mm = String(Math.floor(s / 60) % 60).padStart(2, '0');
  const ss = String(s % 60).padStart(2, '0');
  return s >= 3600 ? `${Math.floor(s / 3600)}:${mm}:${ss}` : `${Math.floor(s / 60)}:${ss}`;
}
