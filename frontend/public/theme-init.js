// Applies the saved theme before first paint to avoid a light/dark flash.
// Kept as a static file (not inline) so the CSP can forbid inline scripts.
(function () {
  try {
    var t = localStorage.getItem("tf-theme") || "system";
    var dark = t === "dark" || (t === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.classList.toggle("dark", dark);
  } catch (e) {
    /* storage unavailable: fall back to light */
  }
})();
