// A save comes back to its page with saved= in the address, and the page
// shows a short success message. The message fades out after a few seconds,
// and saved= leaves the address so a reload does not show it again. Error
// messages stay. Without this script the success message stays too.
(function () {
  "use strict";
  var SHOWN = 4000;
  var FADE = 400;

  var url = new URL(window.location.href);
  if (url.searchParams.has("saved")) {
    url.searchParams.delete("saved");
    window.history.replaceState(window.history.state, "", url.pathname + url.search + url.hash);
  }
  document.querySelectorAll(".msg.flash").forEach(function (m) {
    window.setTimeout(function () {
      m.classList.add("gone");
      window.setTimeout(function () { m.remove(); }, FADE);
    }, SHOWN);
  });
})();
