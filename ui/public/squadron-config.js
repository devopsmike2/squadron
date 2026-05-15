(function () {
  const placeholder = "__SQUADRON_BACKEND_URL__";
  if (typeof window === "undefined") {
    return;
  }

  window.__SQUADRON_CONFIG__ = window.__SQUADRON_CONFIG__ ?? {};

  if (!window.__SQUADRON_CONFIG__.backendUrl) {
    window.__SQUADRON_CONFIG__.backendUrl = placeholder;
  }
})();

