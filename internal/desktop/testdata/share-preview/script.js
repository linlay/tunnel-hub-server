import { filterRows } from "./helper.js";

const results = document.getElementById("results");
function show(name, ok) {
  const row = document.createElement("p");
  row.textContent = `${name}: ${ok ? "PASS" : "FAIL"}`;
  results.append(row);
}
show("module import", typeof filterRows === "function");
try { parent.document.body; show("parent DOM blocked", false); }
catch { show("parent DOM blocked", true); }
try { document.cookie; show("cookie blocked", false); }
catch { show("cookie blocked", true); }
try { parent.desktop; show("parent bridge blocked", false); }
catch { show("parent bridge blocked", true); }
show("own bridge absent", window.desktop === undefined && window.electron === undefined);
show("CSS loaded", getComputedStyle(document.body).borderTopColor === "rgb(12, 88, 133)");
fetch("/api/desktop/v2/shares", { credentials: "include" })
  .then(() => show("management fetch blocked", false))
  .catch(() => show("management fetch blocked", true));
fetch("./data.json").then(response => response.json()).then(data => {
  for (const text of data.rows) {
    const row = document.createElement("li");
    row.dataset.row = "";
    row.textContent = text;
    document.getElementById("rows").append(row);
  }
  show("local JSON loaded", data.rows.length === 2);
}).catch(() => show("local JSON loaded", false));
document.fonts.load("16px Probe").then(() => show("font loaded", document.fonts.check("16px Probe")));
const image = document.getElementById("pixel");
const checkImage = () => show("image loaded", image.complete && image.naturalWidth === 1);
if (image.complete) checkImage(); else image.onload = checkImage;
document.getElementById("filter").oninput = event => filterRows(event.target.value);
document.getElementById("popup").onclick = () => show("popup blocked", window.open("/api/desktop/v2/shares") === null);
document.getElementById("topnav").onclick = () => {
  try { top.location.href = "/api/desktop/v2/shares"; show("top navigation blocked", false); }
  catch { show("top navigation blocked", true); }
};
document.getElementById("selfnav").onclick = () => { location.href = "/api/desktop/v2/shares"; };
document.getElementById("message").onclick = () => {
  let responses = 0;
  const listener = () => { responses += 1; };
  addEventListener("message", listener);
  parent.postMessage({ type: "desktop.action", action: "auth.getToken", probe: true }, "*");
  setTimeout(() => { removeEventListener("message", listener); show("host message ignored", responses === 0); }, 100);
};
const forbiddenImage = new Image();
forbiddenImage.src = "/api/desktop/v2/shares?image=probe";
