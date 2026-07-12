#!/usr/bin/env node
'use strict';
const fs = require('fs');

function main() {
  const inPath = process.argv[2];
  const outPath = process.argv[3];
  if (!inPath || !outPath) { console.error('usage: script <in> <out>'); process.exit(1); }
  const data = JSON.parse(fs.readFileSync(inPath, 'utf8'));
  const fileNodes = data.fileNodes || [];
  const importEdges = data.importEdges || [];
  const allEdges = data.allEdges || [];

  const idToNode = new Map();
  for (const n of fileNodes) idToNode.set(n.id, n);
  const pathOf = (n) => n.filePath || (n.id.includes(':') ? n.id.split(':').slice(1).join(':') : n.id);

  // Common prefix of directory segments
  const codePaths = fileNodes.map(n => pathOf(n));
  function commonDirPrefix(paths) {
    if (paths.length === 0) return '';
    const split = paths.map(p => p.split('/'));
    // only consider directory portion
    let prefix = [];
    const first = split[0];
    for (let i = 0; i < first.length - 1; i++) {
      const seg = first[i];
      if (split.every(s => s.length - 1 > i && s[i] === seg)) prefix.push(seg);
      else break;
    }
    return prefix.length ? prefix.join('/') + '/' : '';
  }
  const prefix = commonDirPrefix(codePaths);

  // A. Directory grouping
  const directoryGroups = {};
  const nodeGroup = {};
  for (const n of fileNodes) {
    let p = pathOf(n);
    if (prefix && p.startsWith(prefix)) p = p.slice(prefix.length);
    const segs = p.split('/');
    const group = segs.length > 1 ? segs[0] : '(root)';
    (directoryGroups[group] = directoryGroups[group] || []).push(n.id);
    nodeGroup[n.id] = group;
  }

  // B. Node type grouping
  const nodeTypeGroups = {};
  for (const n of fileNodes) (nodeTypeGroups[n.type] = nodeTypeGroups[n.type] || []).push(n.id);

  // C. fan-in / fan-out from importEdges
  const fanOut = {}, fanIn = {};
  for (const n of fileNodes) { fanOut[n.id] = 0; fanIn[n.id] = 0; }
  for (const e of importEdges) {
    if (fanOut[e.source] !== undefined) fanOut[e.source]++;
    if (fanIn[e.target] !== undefined) fanIn[e.target]++;
  }

  // D. cross-category edges (allEdges between differing node types)
  const ccMap = {};
  for (const e of allEdges) {
    const s = idToNode.get(e.source), t = idToNode.get(e.target);
    if (!s || !t) continue;
    if (s.type === t.type) continue;
    const key = `${s.type}|${t.type}|${e.type}`;
    ccMap[key] = (ccMap[key] || 0) + 1;
  }
  const crossCategoryEdges = Object.entries(ccMap).map(([k, count]) => {
    const [fromType, toType, edgeType] = k.split('|');
    return { fromType, toType, edgeType, count };
  }).sort((a, b) => b.count - a.count);

  // E. inter-group import frequency (importEdges)
  const igMap = {};
  for (const e of importEdges) {
    const g1 = nodeGroup[e.source], g2 = nodeGroup[e.target];
    if (g1 === undefined || g2 === undefined || g1 === g2) continue;
    const key = `${g1}|${g2}`;
    igMap[key] = (igMap[key] || 0) + 1;
  }
  const interGroupImports = Object.entries(igMap).map(([k, count]) => {
    const [from, to] = k.split('|');
    return { from, to, count };
  }).sort((a, b) => b.count - a.count);

  // F. intra-group density
  const intraGroupDensity = {};
  for (const g of Object.keys(directoryGroups)) intraGroupDensity[g] = { internalEdges: 0, totalEdges: 0, density: 0 };
  for (const e of importEdges) {
    const g1 = nodeGroup[e.source], g2 = nodeGroup[e.target];
    if (g1 !== undefined) intraGroupDensity[g1].totalEdges++;
    if (g2 !== undefined && g2 !== g1) intraGroupDensity[g2].totalEdges++;
    if (g1 !== undefined && g1 === g2) intraGroupDensity[g1].internalEdges++;
  }
  for (const g of Object.keys(intraGroupDensity)) {
    const d = intraGroupDensity[g];
    d.density = d.totalEdges ? +(d.internalEdges / d.totalEdges).toFixed(3) : 0;
  }

  // G. pattern matching
  const dirPatterns = [
    [/^(routes|api|controllers|endpoints|handlers|controller|routers|blueprints|serializers)$/, 'api'],
    [/^(services|core|lib|domain|logic|signals|composables|mailers|jobs|channels|processor|adapters)$/, 'service'],
    [/^(models|db|data|persistence|repository|entities|migrations|entity|sql|database|state|mailcache)$/, 'data'],
    [/^(components|views|pages|ui|layouts|screens)$/, 'ui'],
    [/^(middleware|plugins|interceptors|guards)$/, 'middleware'],
    [/^(utils|helpers|common|shared|tools|pkg|templatetags|fsutil|cryptutil)$/, 'utility'],
    [/^(config|constants|env|settings|management|commands)$/, 'config'],
    [/^(__tests__|test|tests|spec|specs)$/, 'test'],
    [/^(types|interfaces|schemas|contracts|dtos|dto|request|response)$/, 'types'],
    [/^(hooks)$/, 'hooks'],
    [/^(store|reducers|actions|slices)$/, 'state'],
    [/^(assets|static|public|share)$/, 'assets'],
    [/^(cmd|bin|internal|app)$/, 'entry'],
    [/^(docs|documentation|wiki)$/, 'documentation'],
    [/^(deploy|deployment|infra|infrastructure|scripts|docker)$/, 'infrastructure'],
    [/^(\.github|\.gitlab|\.circleci)$/, 'ci-cd'],
  ];
  const patternMatches = {};
  for (const g of Object.keys(directoryGroups)) {
    let label = null;
    for (const [re, lab] of dirPatterns) { if (re.test(g)) { label = lab; break; } }
    if (label) patternMatches[g] = label;
  }

  // H. deployment topology
  const allPaths = fileNodes.map(pathOf);
  const dt = {
    hasDockerfile: allPaths.some(p => /(^|\/)Dockerfile/i.test(p)),
    hasCompose: allPaths.some(p => /docker-compose/i.test(p)),
    hasK8s: allPaths.some(p => /(k8s|kubernetes|helm|charts)/i.test(p)),
    hasTerraform: allPaths.some(p => /\.tf(vars)?$/.test(p)),
    hasCI: allPaths.some(p => /(\.github\/workflows|\.gitlab-ci|Jenkinsfile)/i.test(p)),
    infraFiles: allPaths.filter(p => /(Dockerfile|docker-compose|supervisord|wrangler|\.tf$|entrypoint|bootstrap|start-ollama|pull-ollama)/i.test(p)),
  };

  // I. data pipeline
  const dataPipeline = {
    schemaFiles: allPaths.filter(p => /\.(sql|graphql|gql|proto|prisma)$/i.test(p)),
    migrationFiles: allPaths.filter(p => /migrat/i.test(p)),
    dataModelFiles: allPaths.filter(p => /(models|store|state|mailcache|contacts)/i.test(p) && /\.(go|ts)$/.test(p)),
    apiHandlerFiles: allPaths.filter(p => /(handlers|routes|api)/i.test(p) && /\.(go|ts)$/.test(p)),
  };

  // J. doc coverage
  const groupsWithDocs = new Set();
  for (const n of fileNodes) {
    if (n.type === 'document' || /\.(md|rst)$/i.test(pathOf(n))) groupsWithDocs.add(nodeGroup[n.id]);
  }
  const totalGroups = Object.keys(directoryGroups).length;
  const undoc = Object.keys(directoryGroups).filter(g => !groupsWithDocs.has(g));
  const docCoverage = {
    groupsWithDocs: groupsWithDocs.size,
    totalGroups,
    coverageRatio: +(groupsWithDocs.size / totalGroups).toFixed(2),
    undocumentedGroups: undoc,
  };

  // K. dependency direction
  const pairSeen = new Set();
  const dependencyDirection = [];
  const igLookup = {};
  for (const x of interGroupImports) igLookup[`${x.from}|${x.to}`] = x.count;
  for (const x of interGroupImports) {
    const rev = igLookup[`${x.to}|${x.from}`] || 0;
    const key = [x.from, x.to].sort().join('||');
    if (pairSeen.has(key)) continue;
    pairSeen.add(key);
    if (x.count > rev) dependencyDirection.push({ dependent: x.from, dependsOn: x.to });
    else if (rev > x.count) dependencyDirection.push({ dependent: x.to, dependsOn: x.from });
  }

  const filesPerGroup = {};
  for (const g of Object.keys(directoryGroups)) filesPerGroup[g] = directoryGroups[g].length;
  const nodeTypeCounts = {};
  for (const t of Object.keys(nodeTypeGroups)) nodeTypeCounts[t] = nodeTypeGroups[t].length;

  const out = {
    scriptCompleted: true,
    commonPrefix: prefix,
    directoryGroups,
    nodeTypeGroups,
    crossCategoryEdges,
    interGroupImports,
    intraGroupDensity,
    patternMatches,
    deploymentTopology: dt,
    dataPipeline,
    docCoverage,
    dependencyDirection,
    fileStats: { totalFileNodes: fileNodes.length, filesPerGroup, nodeTypeCounts },
    fileFanIn: fanIn,
    fileFanOut: fanOut,
  };
  fs.writeFileSync(outPath, JSON.stringify(out, null, 2));
  console.error('OK: wrote ' + outPath);
  process.exit(0);
}
try { main(); } catch (e) { console.error(e.stack || String(e)); process.exit(1); }
