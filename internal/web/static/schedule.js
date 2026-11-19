// Schedule rows in Settings. When the period changes, the field it needs
// shows: a weekday for every week, a day of the month for every month. The
// day field takes 1 to 30, and stepping past 30 gives "last day". The server
// renders the saved period's field and checks what is sent, so the page
// works without this script too.
(function () {
  "use strict";
  var LAST = 31;

  function sync(row) {
    var period = row.querySelector("select[data-period]");
    if (!period) return;
    row.querySelectorAll("[data-for]").forEach(function (field) {
      field.hidden = field.getAttribute("data-for") !== period.value;
    });
  }

  function read(input) {
    var v = input.value.trim().toLowerCase();
    if (v.indexOf("last") === 0) return LAST;
    var n = parseInt(v, 10);
    return isNaN(n) ? 1 : Math.min(Math.max(n, 1), 30);
  }

  function show(input, n) {
    input.value = n === LAST ? "last day" : String(n);
  }

  function step(input, by) {
    show(input, Math.min(Math.max(read(input) + by, 1), LAST));
  }

  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.matches && t.matches("select[data-period]")) sync(t.closest(".when"));
    if (t.matches && t.matches("input[data-day]")) show(t, read(t));
  });

  // Typing keeps to digits, and a number above 30 stops at 30.
  document.addEventListener("input", function (e) {
    var t = e.target;
    if (!(t.matches && t.matches("input[data-day]"))) return;
    if (t.value.toLowerCase().indexOf("last") === 0) return;
    var digits = t.value.replace(/[^0-9]/g, "").slice(0, 2);
    if (digits !== "" && parseInt(digits, 10) > 30) digits = "30";
    if (digits !== t.value) t.value = digits;
  });

  document.addEventListener("keydown", function (e) {
    var t = e.target;
    if (!(t.matches && t.matches("input[data-day]"))) return;
    if (e.key === "ArrowUp" || e.key === "ArrowDown") {
      e.preventDefault();
      step(t, e.key === "ArrowUp" ? 1 : -1);
    }
  });

  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("button[data-step]");
    if (!b) return;
    var input = b.closest(".day-field").querySelector("input[data-day]");
    step(input, parseInt(b.getAttribute("data-step"), 10));
    input.focus();
  });
})();
