// Schedule rows in Settings. When the period changes, the field it needs
// shows: a weekday for every week, a day of the month for every month. The
// day field takes 1 to 30, and stepping or typing past 30 gives "Last day".
// Anything else is left as typed and marked invalid, never turned into a
// day. Without this script every field shows, and the server reads only the
// one the period needs and checks it, so the page works without it too.
(function () {
  "use strict";
  var LAST = 31;

  function sync(row) {
    var period = row.querySelector("select[data-period]");
    if (!period) return;
    row.querySelectorAll("[data-for]").forEach(function (field) {
      field.classList.toggle("off", field.getAttribute("data-for") !== period.value);
      field.querySelectorAll("input[data-day]").forEach(check);
    });
  }

  // read is the day the field holds: 1 to 30, LAST for "last" or "last day"
  // in any case or any number past 30, however long, and null for anything
  // else.
  function read(input) {
    var v = input.value.trim().toLowerCase();
    if (v === "last" || v === "last day") return LAST;
    if (!/^[0-9]+$/.test(v)) return null;
    var digits = v.replace(/^0+/, "");
    if (digits.length > 2 || parseInt(digits, 10) > 30) return LAST;
    return digits === "" ? null : parseInt(digits, 10);
  }

  function show(input, n) {
    input.value = n === LAST ? "Last day" : String(n);
    check(input);
  }

  // check marks a field the form must not send. A hidden field is never
  // marked: the browser could not show it, and the server ignores it.
  function check(input) {
    var hidden = input.closest(".off") !== null;
    input.setCustomValidity(hidden || read(input) !== null ? "" : "A day from 1 to 30, or last day");
  }

  function step(input, by) {
    var n = read(input);
    show(input, Math.min(Math.max((n === null ? 0 : n) + by, 1), LAST));
  }

  document.addEventListener("change", function (e) {
    var t = e.target;
    if (t.matches && t.matches("select[data-period]")) sync(t.closest(".when"));
    if (t.matches && t.matches("input[data-day]")) {
      var n = read(t);
      if (n === null) check(t);
      else show(t, n);
    }
  });

  // A number above 30 turns into the last day as it is typed. Words are
  // left as typed until the field is left.
  document.addEventListener("input", function (e) {
    var t = e.target;
    if (!(t.matches && t.matches("input[data-day]"))) return;
    if (/^\s*[0-9]+\s*$/.test(t.value) && read(t) === LAST) show(t, LAST);
    else check(t);
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
