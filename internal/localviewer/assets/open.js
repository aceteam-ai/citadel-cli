// Keeps the viewer token for this tab only (sessionStorage is scoped to this
// origin, which includes the port), then opens the viewer.
"use strict";
sessionStorage.setItem("citadelViewerToken", document.querySelector('meta[name="bearer"]').content);
location.replace("/");
