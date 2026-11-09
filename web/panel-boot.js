// Runs in <head>, before the stylesheet paints anything: it puts the theme
// and the page size the user chose on <html>, so a page forced to light
// never flashes dark and a zoomed page does not load at 100 % and then jump.
// "system" and 100 % need nothing: they are what the markup and the browser
// already give.

// The page sizes of the top bar's control other than 100 %. web/panel.js
// steps through the same list (ZOOM_STEPS) and web/panel.css has one rule
// for each; a test keeps the three in step.
const storedZoomSteps = ["50", "60", "70", "80", "90", "110", "125"];

try {
  const storedTheme = localStorage.getItem("gc_theme");
  const storedZoom = localStorage.getItem("gc_zoom");

  if (storedTheme === "dark" || storedTheme === "light") {
    document.documentElement.dataset.theme = storedTheme;
  }
  if (storedZoomSteps.includes(storedZoom)) {
    document.documentElement.dataset.zoom = storedZoom;
  }
} catch (error) {
  // Storage is blocked (private mode, a strict policy): the page keeps the
  // theme of the system and the browser's own size.
}
