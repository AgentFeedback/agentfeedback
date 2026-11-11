/* Draws the share card from the page's embedded data and saves it as a PNG.
   Everything happens in the browser: no request leaves the page. */
"use strict";
(function () {
  var W = 1200, H = 675;
  var FONT = "system-ui, sans-serif";

  function wrap(ctx, text, width, maxLines) {
    var words = text.split(/\s+/), lines = [], line = "";
    for (var i = 0; i < words.length; i++) {
      var word = words[i];
      // Cut on code points, never inside a surrogate pair.
      var chars = Array.from(word);
      while (ctx.measureText(word).width > width && chars.length > 1) {
        var cut = chars.length - 1;
        while (cut > 1 && ctx.measureText(chars.slice(0, cut).join("")).width > width) cut--;
        if (line) { lines.push(line); line = ""; }
        lines.push(chars.slice(0, cut).join(""));
        chars = chars.slice(cut);
        word = chars.join("");
      }
      var next = line ? line + " " + word : word;
      if (line && ctx.measureText(next).width > width) {
        lines.push(line);
        line = word;
      } else {
        line = next;
      }
    }
    if (line) lines.push(line);
    if (lines.length > maxLines) {
      lines = lines.slice(0, maxLines);
      var last = Array.from(lines[maxLines - 1]);
      while (last.length && ctx.measureText(last.join("") + "…").width > width) last.pop();
      lines[maxLines - 1] = last.join("") + "…";
    }
    return lines;
  }

  function draw(ctx, data) {
    ctx.fillStyle = "#14171c";
    ctx.fillRect(0, 0, W, H);
    ctx.textBaseline = "top";

    ctx.fillStyle = "#f5f6f8";
    ctx.font = "600 34px " + FONT;
    ctx.fillText("Agent checkup", 60, 50);
    ctx.font = "700 150px " + FONT;
    ctx.fillText(data.grade, 60, 100);
    ctx.fillStyle = "#aab2bd";
    ctx.font = "28px " + FONT;
    ctx.fillText(data.score === null ? "no judged sessions" : "score " + data.score, 60, 270);

    var y = 330;
    for (var i = 0; i < data.rubrics.length; i++) {
      var r = data.rubrics[i];
      ctx.fillStyle = "#aab2bd";
      ctx.font = "24px " + FONT;
      ctx.fillText(r.title + (r.score === null ? "  n/a" : ""), 60, y);
      ctx.fillStyle = "#2a2f37";
      ctx.fillRect(60, y + 34, 420, 18);
      if (r.score !== null) {
        ctx.fillStyle = "#4caf7d";
        ctx.fillRect(60, y + 34, 420 * r.score, 18);
      }
      y += 64;
    }

    ctx.fillStyle = "#f5f6f8";
    ctx.font = "600 28px " + FONT;
    ctx.fillText("Top findings", 560, 50);
    ctx.font = "26px " + FONT;
    var fy = 100;
    for (var j = 0; j < data.findings.length; j++) {
      var lines = wrap(ctx, (j + 1) + ". " + data.findings[j], 580, 4);
      for (var k = 0; k < lines.length; k++) {
        ctx.fillText(lines[k], 560, fy);
        fy += 34;
      }
      fy += 22;
    }

    ctx.fillStyle = "#aab2bd";
    ctx.font = "24px " + FONT;
    ctx.textAlign = "right";
    ctx.fillText("agentfeedback.dev", W - 60, H - 60);
  }

  document.addEventListener("DOMContentLoaded", function () {
    var button = document.getElementById("share");
    var source = document.getElementById("card-data");
    if (!button || !source) return;
    var data = JSON.parse(source.textContent);
    button.hidden = false;
    button.addEventListener("click", function () {
      var canvas = document.createElement("canvas");
      canvas.width = W;
      canvas.height = H;
      draw(canvas.getContext("2d"), data);
      canvas.toBlob(function (blob) {
        if (!blob) return;
        var blobURL = URL.createObjectURL(blob);
        var a = document.createElement("a");
        a.href = blobURL;
        a.download = "agentfeedback-checkup.png";
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(function () { URL.revokeObjectURL(blobURL); }, 1000);
      }, "image/png");
    });
  });
})();
