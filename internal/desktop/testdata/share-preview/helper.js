export function filterRows(value) {
  for (const row of document.querySelectorAll("[data-row]")) {
    row.hidden = !row.textContent.toLowerCase().includes(value.toLowerCase());
  }
}
