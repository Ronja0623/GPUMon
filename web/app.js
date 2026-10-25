/* GPUMon dashboard. Plain JavaScript, no build step, no external requests.
 * All text is built with textContent, never innerHTML, because owner names
 * can come from container labels. */
(function () {
  "use strict";

  var CFG = { timezone: "UTC", timezone_label: "UTC", refresh_seconds: 15, public_history_hours: 72 };
  var HISTORY_EVERY_MS = 60000;
  var charts = [];
  var lastHistory = 0;

  var PALETTE = ["#3b82f6", "#f97316", "#10b981", "#a855f7", "#ef4444", "#14b8a6", "#eab308", "#ec4899",
    "#6366f1", "#84cc16", "#06b6d4", "#d946ef", "#f43f5e", "#22c55e", "#0ea5e9", "#f59e0b"];
  var SPECIAL = { "system": "#9ca3af", "root-shared": "#6b7280", "unattributed": "#a8a29e", "others": "#cbd5e1" };

  function isSpecial(o) { return o === "system" || o === "root-shared" || o === "unattributed" || o.indexOf("uid:") === 0; }

  function colorFor(owner) {
    if (SPECIAL[owner]) { return SPECIAL[owner]; }
    if (owner.indexOf("uid:") === 0) { return "#78716c"; }
    var h = 0;
    for (var i = 0; i < owner.length; i++) { h = (h * 31 + owner.charCodeAt(i)) >>> 0; }
    return PALETTE[h % PALETTE.length];
  }

  function el(tag, attrs) {
    var e = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === "text") { e.textContent = attrs[k]; } else if (k === "cls") { e.className = attrs[k]; } else { e.setAttribute(k, attrs[k]); }
      });
    }
    for (var i = 2; i < arguments.length; i++) {
      var c = arguments[i];
      if (c === null || c === undefined || c === false) { continue; }
      e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    }
    return e;
  }

  function fmtBytes(b) {
    if (b === null || b === undefined) { return "n/a"; }
    var u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    var i = 0;
    while (Math.abs(b) >= 1024 && i < u.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b.toFixed(0) : b.toFixed(b >= 100 ? 0 : 1)) + " " + u[i];
  }
  function fmtMiB(m) { return m === null || m === undefined ? "n/a" : fmtBytes(m * 1048576); }
  function fmtPct(p) { return p === null || p === undefined ? "n/a" : Math.round(p) + " %"; }

  var dtf = null;
  function fmtTime(value) {
    if (!value) { return "never"; }
    var d = typeof value === "number" ? new Date(value * 1000) : new Date(value);
    if (!dtf) {
      dtf = new Intl.DateTimeFormat("en-GB", { timeZone: CFG.timezone, year: "numeric", month: "2-digit", day: "2-digit",
        hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
    }
    var p = {};
    dtf.formatToParts(d).forEach(function (x) { p[x.type] = x.value; });
    return p.year + "-" + p.month + "-" + p.day + " " + p.hour + ":" + p.minute + ":" + p.second;
  }

  function badge(state) {
    var label = { free: "free", busy: "busy", idle_occupied: "idle (occupied)", unknown: "unknown" }[state] || state;
    return el("span", { cls: "badge state-" + state, text: label, title: "GPU state: " + label });
  }

  function ownerSpan(o) {
    return el("span", { cls: "owner" + (isSpecial(o) ? " special" : ""), text: o,
      title: isSpecial(o) ? "Special owner: not a personal account" : "Owner" });
  }

  function fetchJSON(url) {
    return fetch(url, { cache: "no-store" }).then(function (r) {
      return r.json().then(function (body) {
        if (!r.ok) { throw new Error(body && body.error ? body.error : "HTTP " + r.status); }
        return body;
      });
    });
  }

  /* ---------------- live view ---------------- */

  function renderSummary(status) {
    var tbody = document.querySelector("#summary tbody");
    tbody.textContent = "";
    if (!status.summary.length) {
      tbody.appendChild(el("tr", null, el("td", { colspan: "6", text: "No GPUs reported yet." })));
    }
    status.summary.forEach(function (g) {
      var owners = el("td");
      if (!g.owners.length) { owners.appendChild(el("span", { cls: "method", text: "none" })); }
      g.owners.forEach(function (o, i) { if (i) { owners.appendChild(document.createTextNode(", ")); } owners.appendChild(ownerSpan(o)); });
      tbody.appendChild(el("tr", null,
        el("td", null, badge(g.state)),
        el("td", { text: g.display_name }),
        el("td", { text: "GPU " + g.gpu_index + ": " + (g.gpu_name || "unknown model") }),
        el("td", { cls: "num", text: fmtMiB(g.vram_free_mib) + " / " + fmtMiB(g.vram_total_mib) }),
        el("td", { cls: "num", text: fmtPct(g.utilization_pct) }),
        owners));
    });
  }

  function segmentBar(parts, total, title) {
    var bar = el("div", { cls: "bar", role: "img", "aria-label": title });
    parts.forEach(function (p) {
      if (!total || p.value <= 0) { return; }
      var s = el("span", { title: p.label + ": " + p.text });
      s.style.width = (100 * p.value / total).toFixed(3) + "%";
      s.style.background = p.color;
      bar.appendChild(s);
    });
    return bar;
  }

  function renderGPU(g) {
    var box = el("div", { cls: "gpu" });
    box.appendChild(el("div", { cls: "gpu-head" },
      el("span", { cls: "name", text: "GPU " + g.index + ": " + (g.name || "unknown model") }),
      badge(g.state),
      el("span", { cls: "gpu-meta", text: "utilization " + fmtPct(g.utilization_pct) +
        (g.temperature_c !== null && g.temperature_c !== undefined ? ", " + g.temperature_c + " C" : "") +
        (g.power_draw_w !== null && g.power_draw_w !== undefined ? ", " + Math.round(g.power_draw_w) + " W" : "") })));
    var parts = [];
    var ownersVram = 0;
    g.owners.forEach(function (o) { ownersVram += o.vram_mib; parts.push({ label: o.owner, value: o.vram_mib, text: fmtMiB(o.vram_mib), color: colorFor(o.owner) }); });
    var other = Math.max(0, (g.vram_used_mib || 0) - ownersVram);
    parts.push({ label: "other usage", value: other, text: fmtMiB(other), color: "#94a3b8" });
    box.appendChild(segmentBar(parts, g.vram_total_mib || 0, "VRAM usage"));
    box.appendChild(el("div", { cls: "bar-label", text: "VRAM " + fmtMiB(g.vram_used_mib) + " used of " + fmtMiB(g.vram_total_mib) }));
    if (g.owners.length) {
      var ul = el("ul");
      g.owners.forEach(function (o) {
        var where = [];
        if (o.containers.length) { where.push("container " + o.containers.join(", ")); }
        if (o.vms.length) { where.push("VM " + o.vms.join(", ")); }
        ul.appendChild(el("li", null, ownerSpan(o.owner), " " + fmtMiB(o.vram_mib) + " ",
          el("span", { cls: "method", text: "via " + o.methods.join(", ") + (where.length ? "; " + where.join("; ") : "") +
            "; programs: " + o.exes.join(", ") })));
      });
      box.appendChild(ul);
    }
    return box;
  }

  function renderUsers(users) {
    var wrap = el("div", { cls: "table-wrap" });
    var t = el("table", null, el("thead", null, el("tr", null,
      el("th", { text: "Owner" }), el("th", { cls: "num", text: "CPU cores" }), el("th", { cls: "num", text: "RSS" }),
      el("th", { cls: "num", text: "Processes" }), el("th", { text: "Attribution" }))));
    var tb = el("tbody");
    users.forEach(function (u) {
      var top = u.top_cpu.slice(0, 3).map(function (p) { return p.exe + (p.vm_name ? " (VM " + p.vm_name + ")" : p.container_name ? " (" + p.container_name + ")" : ""); });
      tb.appendChild(el("tr", { title: "Top processes: " + top.join(", ") },
        el("td", null, ownerSpan(u.owner)),
        el("td", { cls: "num", text: u.cpu_cores.toFixed(2) }),
        el("td", { cls: "num", text: fmtBytes(u.rss_bytes) }),
        el("td", { cls: "num", text: String(u.process_count) }),
        el("td", { cls: "method", text: u.attributions.join(", ") })));
    });
    t.appendChild(tb);
    wrap.appendChild(t);
    return wrap;
  }

  function renderDisk(fs) {
    var box = el("div", { cls: "disk" });
    var asOf = fs.as_of ? "per-owner scan as of " + fmtTime(fs.as_of) : "no per-owner scan yet";
    box.appendChild(el("div", null, el("strong", { text: fs.mount }), " ",
      el("span", { cls: "method", text: fmtBytes(fs.used_bytes) + " used of " + fmtBytes(fs.size_bytes) + ", " + asOf })));
    if (fs.partial) {
      box.appendChild(el("div", { cls: "warning", text: "Warning: the last scan stopped at its time limit; per-owner numbers are partial." }));
    }
    var total = 0;
    fs.segments.forEach(function (s) { total += s.bytes; });
    var parts = fs.segments.map(function (s) {
      var color = s.kind === "owner" ? colorFor(s.label) : s.kind === "others" ? "#cbd5e1" : s.kind === "unscanned" ? "#94a3b8" : "transparent";
      return { label: s.label, value: s.bytes, text: fmtBytes(s.bytes), color: color };
    });
    box.appendChild(segmentBar(parts, total, "Disk usage of " + fs.mount));
    var legend = el("div", { cls: "legend" });
    parts.forEach(function (p) {
      if (p.value <= 0) { return; }
      var sw = el("i");
      sw.style.background = p.color === "transparent" ? "var(--unknown-bg)" : p.color;
      legend.appendChild(el("span", null, sw, p.label + " " + p.text));
    });
    box.appendChild(legend);
    return box;
  }

  function renderServers(status) {
    var root = document.getElementById("servers");
    root.textContent = "";
    if (!status.servers.length) { root.appendChild(el("p", { cls: "hint", text: "No server has reported yet." })); }
    status.servers.forEach(function (s) {
      var card = el("article", { cls: "card", id: "server-" + s.server_id });
      card.appendChild(el("h3", null, s.display_name,
        el("span", { cls: "badge status-" + s.status, text: s.status }),
        el("span", { cls: "lastseen", text: "last seen " + fmtTime(s.last_seen) })));
      if (s.notes) { card.appendChild(el("p", { cls: "notes", text: s.notes })); }
      if (s.clock_skew_warning) {
        card.appendChild(el("div", { cls: "warning", text: "Clock skew: the agent clock differs from central by " + s.clock_skew_s + " s. Check NTP (timedatectl)." }));
      }
      if (s.host) {
        var h = s.host;
        card.appendChild(el("div", { cls: "host" },
          el("div", null, el("span", { text: "CPU" }), fmtPct(h.cpu_percent) + " of " + h.logical_cores + " cores"),
          el("div", null, el("span", { text: "Load (1/5/15 min)" }), h.load_avg.map(function (x) { return x.toFixed(2); }).join(" ")),
          el("div", null, el("span", { text: "Memory" }), fmtBytes(h.mem_used_bytes) + " / " + fmtBytes(h.mem_total_bytes)),
          el("div", null, el("span", { text: "Swap" }), fmtBytes(h.swap_used_bytes) + " / " + fmtBytes(h.swap_total_bytes)),
          el("div", null, el("span", { text: "Clock skew" }), s.clock_skew_s === null ? "n/a" : s.clock_skew_s + " s")));
      }
      card.appendChild(el("h4", { text: "GPUs" }));
      if (!s.gpus.length) { card.appendChild(el("p", { cls: "hint", text: "No GPU detected." })); }
      s.gpus.forEach(function (g) { card.appendChild(renderGPU(g)); });
      card.appendChild(el("h4", { text: "CPU and memory by owner" }));
      card.appendChild(renderUsers(s.users));
      card.appendChild(el("h4", { text: "Disk" }));
      s.disk.forEach(function (fs) { card.appendChild(renderDisk(fs)); });
      if (s.errors && s.errors.length) {
        var d = el("details", { cls: "errors" }, el("summary", { text: s.errors.length + " collection problem(s) reported by the agent" }));
        s.errors.forEach(function (e) { d.appendChild(el("div", { text: e })); });
        card.appendChild(d);
      }
      root.appendChild(card);
    });
  }

  /* ---------------- history charts ---------------- */

  function gapPlugin(gaps, tip) {
    return {
      hooks: {
        drawClear: [function (u) {
          var ctx = u.ctx;
          ctx.save();
          ctx.fillStyle = getComputedStyle(document.documentElement).getPropertyValue("--gap") || "rgba(128,128,128,0.2)";
          gaps.forEach(function (g) {
            var x0 = Math.max(u.bbox.left, u.valToPos(g.start, "x", true));
            var x1 = Math.min(u.bbox.left + u.bbox.width, u.valToPos(g.end, "x", true));
            if (x1 > x0) { ctx.fillRect(x0, u.bbox.top, Math.max(2, x1 - x0), u.bbox.height); }
          });
          ctx.restore();
        }],
        setCursor: [function (u) {
          if (u.cursor.left === null || u.cursor.left < 0) { tip.textContent = ""; return; }
          var x = u.posToVal(u.cursor.left, "x");
          var hit = gaps.filter(function (g) { return g.start <= x && x <= g.end; })[0];
          tip.textContent = hit ? hit.description : "";
        }]
      }
    };
  }

  function stackedChart(parent, title, ts, byOwner, fmt, gaps, line) {
    var box = el("div", { cls: "chart" });
    box.appendChild(el("div", { cls: "chart-title", text: title }));
    var holder = el("div");
    var tip = el("div", { cls: "gap-tip" });
    box.appendChild(holder);
    box.appendChild(tip);
    parent.appendChild(box);
    var owners = Object.keys(byOwner);
    var raw = owners.map(function (o) { return byOwner[o]; });
    var cum = [];
    raw.forEach(function (vals, i) {
      cum.push(vals.map(function (v, j) {
        if (v === null) { return null; }
        return (i > 0 && cum[i - 1][j] !== null ? cum[i - 1][j] : 0) + v;
      }));
    });
    var data = [ts];
    var series = [{}];
    for (var i = owners.length - 1; i >= 0; i--) {
      (function (idx) {
        var c = colorFor(owners[idx]);
        data.push(cum[idx]);
        series.push({ label: owners[idx], stroke: c, fill: c + "99", width: 1,
          value: function (u, v, si, di) { var r = raw[idx][di]; return r === null || r === undefined ? "--" : fmt(r); } });
      })(i);
    }
    if (line) {
      data.push(line.values);
      series.push({ label: line.label, stroke: "#111827", width: 1.5, dash: [4, 3],
        value: function (u, v) { return v === null || v === undefined ? "--" : fmt(v); } });
    }
    var opts = {
      width: Math.max(260, holder.clientWidth || parent.clientWidth || 300),
      height: 180,
      series: series,
      tzDate: function (t) { return uPlot.tzDate(new Date(t * 1000), CFG.timezone); },
      axes: [{}, { size: 60, values: function (u, vals) { return vals.map(function (v) { return fmt(v); }); } }],
      scales: { x: { time: true } },
      legend: { live: true },
      plugins: [gapPlugin(gaps, tip)]
    };
    var chart = new uPlot(opts, data, holder);
    charts.push({ chart: chart, holder: holder });
  }

  function renderHistory(h) {
    charts.forEach(function (c) { c.chart.destroy(); });
    charts = [];
    var root = document.getElementById("history");
    root.textContent = "";
    var ts = h.timestamps;
    h.servers.forEach(function (s) {
      var sec = el("div", { cls: "hist-server" });
      sec.appendChild(el("h3", { text: s.display_name }));
      var grid = el("div", { cls: "charts" });
      sec.appendChild(grid);
      root.appendChild(sec);
      var gaps = h.gaps.filter(function (g) { return g.server_id === s.server_id; });
      stackedChart(grid, "CPU cores by owner", ts, s.cpu_by_owner, function (v) { return Number(v).toFixed(1); }, gaps);
      stackedChart(grid, "Memory (RSS) by owner", ts, s.mem_by_owner, fmtBytes, gaps);
      s.gpus.forEach(function (g) {
        var name = "GPU " + g.index + (g.name ? " " + g.name : "");
        stackedChart(grid, name + ": VRAM by owner", ts, g.vram_by_owner, fmtMiB, gaps);
        var hasPerOwner = Object.keys(g.util_by_owner).length > 0;
        stackedChart(grid, name + ": utilization" + (hasPerOwner ? " by owner (driver-reported)" : " (device total)"), ts,
          hasPerOwner ? g.util_by_owner : {}, function (v) { return Math.round(v) + "%"; }, gaps,
          { label: "device", values: g.util });
      });
    });
    if (!h.servers.length) { root.appendChild(el("p", { cls: "hint", text: "No history yet." })); }
  }

  window.addEventListener("resize", function () {
    charts.forEach(function (c) { c.chart.setSize({ width: Math.max(260, c.holder.clientWidth), height: 180 }); });
  });

  /* ---------------- refresh loop ---------------- */

  function refresh() {
    fetchJSON("api/v1/status").then(function (s) {
      renderSummary(s);
      renderServers(s);
      document.getElementById("updated").textContent = "Updated " + fmtTime(s.generated_at) + ", refreshes every " + CFG.refresh_seconds + " s";
    }).catch(function (e) {
      document.getElementById("updated").textContent = "Update failed (" + e.message + "); retrying";
    });
    if (Date.now() - lastHistory >= HISTORY_EVERY_MS) {
      lastHistory = Date.now();
      fetchJSON("api/v1/history").then(renderHistory).catch(function (e) {
        document.getElementById("history").textContent = "History unavailable: " + e.message;
      });
    }
  }

  fetchJSON("api/v1/config").then(function (c) {
    CFG = c;
  }).catch(function () { /* keep defaults */ }).then(function () {
    document.getElementById("tz").textContent = "Times in " + CFG.timezone_label;
    document.getElementById("history-hint").firstChild.textContent = "Last " + CFG.public_history_hours +
      " hours. Shaded areas are data gaps; hover them for details. Per-user GPU utilization is only shown when the driver reports it; otherwise the device total is shown.";
    refresh();
    setInterval(refresh, CFG.refresh_seconds * 1000);
  });
})();
