/* Shows each time element in the browser's local time; the page works without it. */
"use strict";
document.addEventListener("DOMContentLoaded", function () {
  var times = document.querySelectorAll("time[datetime]");
  for (var i = 0; i < times.length; i++) {
    var d = new Date(times[i].getAttribute("datetime"));
    if (!isNaN(d.getTime())) {
      times[i].title = times[i].getAttribute("datetime");
      times[i].textContent = d.toLocaleString();
    }
  }
});
