#!/usr/bin/env node
'use strict';

const fs = require('fs');

function main() {
  const inputPath = process.argv[2];
  const outputPath = process.argv[3];
  if (!inputPath || !outputPath) {
    console.error('Usage: node ua-tour-analyze.js <input.json> <output.json>');
    process.exit(1);
  }

  const raw = fs.readFileSync(inputPath, 'utf8');
  const data = JSON.parse(raw);
  const nodes = data.nodes || [];
  const edges = data.edges || [];
  const layers = data.layers || [];

  // Node set of real (provided) node ids
  const nodeById = new Map();
  for (const n of nodes) nodeById.set(n.id, n);

  const nameOf = (id) => (nodeById.get(id) ? nodeById.get(id).name : id);
  const summaryOf = (id) => (nodeById.get(id) ? nodeById.get(id).summary || '' : '');

  // Fan-in / fan-out counting. Only count edges between provided nodes for
  // rankings, but keep imports/calls for BFS separately.
  const fanIn = new Map();
  const fanOut = new Map();
  for (const n of nodes) { fanIn.set(n.id, 0); fanOut.set(n.id, 0); }

  for (const e of edges) {
    const s = e.source, t = e.target;
    if (nodeById.has(s) && nodeById.has(t)) {
      fanOut.set(s, fanOut.get(s) + 1);
      fanIn.set(t, fanIn.get(t) + 1);
    }
  }

  const fanInRanking = [...nodeById.keys()]
    .map((id) => ({ id, fanIn: fanIn.get(id), name: nameOf(id) }))
    .sort((a, b) => b.fanIn - a.fanIn)
    .slice(0, 20);

  const fanOutRanking = [...nodeById.keys()]
    .map((id) => ({ id, fanOut: fanOut.get(id), name: nameOf(id) }))
    .sort((a, b) => b.fanOut - a.fanOut)
    .slice(0, 20);

  // --- Entry point candidates ---
  const entryNames = new Set([
    'index.ts', 'index.js', 'main.ts', 'main.js', 'app.ts', 'app.js',
    'server.ts', 'server.js', 'mod.rs', 'main.go', 'main.py', 'main.rs',
    'manage.py', 'app.py', 'wsgi.py', 'asgi.py', 'run.py', '__main__.py',
    'Application.java', 'Main.java', 'Program.cs', 'config.ru', 'index.php',
    'App.swift', 'Application.kt', 'main.cpp', 'main.c'
  ]);

  const fanOutValues = [...fanOut.values()].sort((a, b) => b - a);
  const top10pctIdx = Math.max(0, Math.floor(fanOutValues.length * 0.1) - 1);
  const top10pctThreshold = fanOutValues.length ? fanOutValues[top10pctIdx] : 0;
  const fanInValues = [...fanIn.values()].sort((a, b) => a - b);
  const bottom25Idx = Math.max(0, Math.floor(fanInValues.length * 0.25) - 1);
  const bottom25Threshold = fanInValues.length ? fanInValues[bottom25Idx] : 0;

  const entryScores = [];
  for (const n of nodes) {
    let score = 0;
    const fp = n.filePath || '';
    const depth = fp ? fp.split('/').length : 99;
    if (n.type === 'document') {
      if (n.name === 'README.md' && depth === 1) score += 5;
      else if (/\.md$/i.test(n.name || '') && depth === 1) score += 2;
    } else if (n.type === 'file') {
      if (entryNames.has(n.name)) score += 3;
      if (depth <= 2) score += 1;
      if (fanOut.get(n.id) >= top10pctThreshold && top10pctThreshold > 0) score += 1;
      if (fanIn.get(n.id) <= bottom25Threshold) score += 1;
    }
    if (score > 0) {
      entryScores.push({ id: n.id, score, name: n.name, summary: n.summary || '' });
    }
  }
  entryScores.sort((a, b) => b.score - a.score);
  const entryPointCandidates = entryScores.slice(0, 5);

  // --- BFS from top code entry point ---
  // Build forward adjacency for imports/calls among provided file nodes.
  // Edges may reference function:/class: nodes; map those calls back is not
  // needed for file-level BFS, so we only follow file->file imports/calls.
  const adj = new Map();
  for (const n of nodes) adj.set(n.id, []);
  for (const e of edges) {
    if ((e.type === 'imports' || e.type === 'calls') && nodeById.has(e.source) && nodeById.has(e.target)) {
      adj.get(e.source).push(e.target);
    }
  }

  // pick top code entry (skip documents)
  let startNode = null;
  for (const c of entryScores) {
    if (nodeById.get(c.id).type !== 'document') { startNode = c.id; break; }
  }
  if (!startNode) {
    // fallback: highest fan-out file
    const f = fanOutRanking.find((x) => nodeById.get(x.id).type === 'file');
    startNode = f ? f.id : (nodes[0] && nodes[0].id);
  }

  const order = [];
  const depthMap = {};
  if (startNode) {
    const visited = new Set([startNode]);
    const queue = [startNode];
    depthMap[startNode] = 0;
    while (queue.length) {
      const cur = queue.shift();
      order.push(cur);
      for (const nxt of (adj.get(cur) || [])) {
        if (!visited.has(nxt)) {
          visited.add(nxt);
          depthMap[nxt] = depthMap[cur] + 1;
          queue.push(nxt);
        }
      }
    }
  }
  const byDepth = {};
  for (const id of order) {
    const d = String(depthMap[id]);
    if (!byDepth[d]) byDepth[d] = [];
    byDepth[d].push(id);
  }

  // --- Non-code file inventory ---
  const nonCodeFiles = { documentation: [], infrastructure: [], data: [], config: [] };
  for (const n of nodes) {
    const entry = { id: n.id, name: n.name, type: n.type, summary: n.summary || '' };
    if (n.type === 'document') nonCodeFiles.documentation.push(entry);
    else if (n.type === 'service' || n.type === 'pipeline' || n.type === 'resource') nonCodeFiles.infrastructure.push(entry);
    else if (n.type === 'table' || n.type === 'schema' || n.type === 'endpoint') nonCodeFiles.data.push(entry);
    else if (n.type === 'config') nonCodeFiles.config.push(entry);
  }

  // --- Clusters (bidirectional pairs, expanded) ---
  const edgeSet = new Set();
  for (const e of edges) {
    if ((e.type === 'imports' || e.type === 'calls') && nodeById.has(e.source) && nodeById.has(e.target)) {
      edgeSet.add(e.source + '|' + e.target);
    }
  }
  const undirectedCount = new Map(); // pairKey -> edge count between the two
  const neighbors = new Map();
  for (const n of nodes) neighbors.set(n.id, new Set());
  for (const e of edges) {
    if ((e.type === 'imports' || e.type === 'calls') && nodeById.has(e.source) && nodeById.has(e.target) && e.source !== e.target) {
      neighbors.get(e.source).add(e.target);
      neighbors.get(e.target).add(e.source);
    }
  }
  const bidiPairs = [];
  const seenPair = new Set();
  for (const e of edges) {
    const s = e.source, t = e.target;
    if (!nodeById.has(s) || !nodeById.has(t)) continue;
    if (edgeSet.has(s + '|' + t) && edgeSet.has(t + '|' + s)) {
      const key = [s, t].sort().join('|');
      if (!seenPair.has(key)) { seenPair.add(key); bidiPairs.push([s, t]); }
    }
  }
  const clusters = [];
  for (const [a, b] of bidiPairs) {
    const cluster = new Set([a, b]);
    // expand: add nodes connected to 2+ members
    for (const n of nodes) {
      if (cluster.has(n.id)) continue;
      let hits = 0;
      for (const m of cluster) if (neighbors.get(n.id).has(m)) hits++;
      if (hits >= 2 && cluster.size < 5) cluster.add(n.id);
    }
    let edgeCount = 0;
    const arr = [...cluster];
    for (let i = 0; i < arr.length; i++) {
      for (let j = 0; j < arr.length; j++) {
        if (i !== j && edgeSet.has(arr[i] + '|' + arr[j])) edgeCount++;
      }
    }
    clusters.push({ nodes: arr, edgeCount });
  }
  clusters.sort((a, b) => b.edgeCount - a.edgeCount);
  const topClusters = clusters.slice(0, 10);

  // --- Node summary index (all provided nodes) ---
  const nodeSummaryIndex = {};
  for (const n of nodes) {
    nodeSummaryIndex[n.id] = { name: n.name, type: n.type, summary: n.summary || '' };
  }

  const result = {
    scriptCompleted: true,
    entryPointCandidates,
    fanInRanking,
    fanOutRanking,
    bfsTraversal: { startNode, order, depthMap, byDepth },
    nonCodeFiles,
    clusters: topClusters,
    layers: { count: layers.length, list: layers.map((l) => ({ id: l.id, name: l.name, description: l.description })) },
    nodeSummaryIndex,
    totalNodes: nodes.length,
    totalEdges: edges.length
  };

  fs.writeFileSync(outputPath, JSON.stringify(result, null, 2));
  process.exit(0);
}

try { main(); } catch (err) {
  console.error('Fatal error:', err && err.stack ? err.stack : err);
  process.exit(1);
}
