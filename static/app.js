// Progressive enhancement only: every page works without this file.

// Show times in the viewer's own timezone, and keep countdowns fresh.
function untilText(t) {
  const ms = t - Date.now();
  if (ms <= 0) return "closing…";
  const m = Math.ceil(ms / 60000), h = Math.floor(m / 60);
  if (m < 60) return m + " min";
  if (h < 48) return h + "h " + (m % 60) + "m";
  return Math.floor(h / 24) + " days";
}
function refreshTimes() {
  document.querySelectorAll("time[data-local]").forEach(el => {
    el.textContent = new Date(el.dateTime).toLocaleString(undefined,
      { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  });
  document.querySelectorAll("time[data-countdown]").forEach(el => {
    el.textContent = untilText(new Date(el.dateTime));
  });
}

// Confirm dialogs for destructive buttons.
document.addEventListener("submit", e => {
  const msg = e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
}, true);

// Resize photos on-device before upload: phones produce 5–10MB images, and
// drawing through createImageBitmap applies EXIF rotation so nothing ends up sideways.
const MAX_DIM = 1600;
async function shrink(file) {
  if (!file.type.startsWith("image/") || file.type === "image/gif") return file;
  try {
    const bmp = await createImageBitmap(file, { imageOrientation: "from-image" });
    const scale = Math.min(1, MAX_DIM / Math.max(bmp.width, bmp.height));
    const canvas = document.createElement("canvas");
    canvas.width = Math.round(bmp.width * scale);
    canvas.height = Math.round(bmp.height * scale);
    canvas.getContext("2d").drawImage(bmp, 0, 0, canvas.width, canvas.height);
    const blob = await new Promise(r => canvas.toBlob(r, "image/jpeg", 0.85));
    if (!blob || (scale === 1 && blob.size > file.size)) return file;
    return new File([blob], file.name.replace(/\.\w+$/, "") + ".jpg", { type: "image/jpeg" });
  } catch {
    return file; // e.g. a format this browser can't decode; let the server decide
  }
}

function setupUpload(form) {
  const input = form.querySelector("input[data-resize]");
  const previews = form.querySelector("[data-previews]");
  const submit = form.querySelector("[data-submit]");
  if (!input || typeof DataTransfer === "undefined") return;
  input.addEventListener("change", async () => {
    const label = submit.textContent;
    submit.disabled = true;
    submit.textContent = "Preparing photos…";
    const dt = new DataTransfer();
    for (const f of await Promise.all([...input.files].map(shrink))) dt.items.add(f);
    input.files = dt.files;
    previews.replaceChildren(...[...dt.files].map(f => {
      const img = document.createElement("img");
      img.src = URL.createObjectURL(f);
      return img;
    }));
    submit.disabled = false;
    submit.textContent = label;
  });
  form.addEventListener("submit", () => {
    submit.disabled = true;
    submit.textContent = "Uploading…";
  });

  // Only show lottery length when the lottery mode is selected.
  const opts = form.querySelector("[data-lottery-opts]");
  const sync = () => { opts.hidden = form.querySelector("input[name=mode]:checked")?.value !== "lottery"; };
  form.querySelectorAll("input[name=mode]").forEach(r => r.addEventListener("change", sync));
  sync();
}

document.addEventListener("DOMContentLoaded", () => {
  refreshTimes();
  setInterval(refreshTimes, 30000);
  document.querySelectorAll("form[data-upload]").forEach(setupUpload);
});
