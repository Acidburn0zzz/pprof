// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package graph collects a set of samples into a directed graph.
package graph

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/pprof/profile"
)

// Graph summarizes a performance profile into a format that is
// suitable for visualization.
type Graph struct {
	Nodes Nodes
}

// Options encodes the options for constructing a graph
type Options struct {
	SampleValue func(s []int64) int64      // Function to compute the value of a sample
	FormatTag   func(int64, string) string // Function to format a sample tag value into a string
	ObjNames    bool                       // Always preserve obj filename

	CallTree     bool // Build a tree instead of a graph
	DropNegative bool // Drop nodes with overall negative values

	KeptNodes NodeSet // If non-nil, only use nodes in this set
}

// Nodes is an ordered collection of graph nodes.
type Nodes []*Node

// Node is an entry on a profiling report. It represents a unique
// program location.
type Node struct {
	// Information associated to this entry.
	Info NodeInfo

	// values associated to this node.
	// Flat is exclusive to this node, cum includes all descendents.
	Flat, Cum int64

	// in and out contains the nodes immediately reaching or reached by this nodes.
	In, Out EdgeMap

	// tags provide additional information about subsets of a sample.
	LabelTags TagMap

	// Numeric tags provide additional values for subsets of a sample.
	// Numeric tags are optionally associated to a label tag. The key
	// for NumericTags is the name of the LabelTag they are associated
	// to, or "" for numeric tags not associated to a label tag.
	NumericTags map[string]TagMap
}

// AddToEdge increases the weight of an edge between two nodes. If
// there isn't such an edge one is created.
func (n *Node) AddToEdge(to *Node, w int64, residual, inline bool) {
	if n.Out[to] != to.In[n] {
		panic(fmt.Errorf("asymmetric edges %v %v", *n, *to))
	}

	if e := n.Out[to]; e != nil {
		e.Weight += w
		if residual {
			e.Residual = true
		}
		if !inline {
			e.Inline = false
		}
		return
	}

	info := &Edge{Src: n, Dest: to, Weight: w, Residual: residual, Inline: inline}
	n.Out[to] = info
	to.In[n] = info
}

// NodeInfo contains the attributes for a node.
type NodeInfo struct {
	Name              string
	OrigName          string
	Address           uint64
	File              string
	StartLine, Lineno int
	Objfile           string
}

// PrintableName calls the Node's Formatter function with a single space separator.
func (i *NodeInfo) PrintableName() string {
	return strings.Join(i.NameComponents(), " ")
}

// NameComponents returns the components of the printable name to be used for a node.
func (i *NodeInfo) NameComponents() []string {
	var name []string
	if i.Address != 0 {
		name = append(name, fmt.Sprintf("%016x", i.Address))
	}
	if fun := i.Name; fun != "" {
		name = append(name, fun)
	}

	switch {
	case i.Lineno != 0:
		// User requested line numbers, provide what we have.
		name = append(name, fmt.Sprintf("%s:%d", i.File, i.Lineno))
	case i.File != "":
		// User requested file name, provide it.
		name = append(name, i.File)
	case i.Name != "":
		// User requested function name. It was already included.
	case i.Objfile != "":
		// Only binary name is available
		name = append(name, "["+i.Objfile+"]")
	default:
		// Do not leave it empty if there is no information at all.
		name = append(name, "<unknown>")
	}
	return name
}

// NodeMap maps from a node info struct to a node. It is used to merge
// report entries with the same info.
type NodeMap map[NodeInfo]*Node

// NodeSet maps is a collection of node info structs.
type NodeSet map[NodeInfo]bool

// FindOrInsertNode takes the info for a node and either returns a matching node
// from the node map if one exists, or adds one to the map if one does not.
// If kept is non-nil, nodes are only added if they can be located on it.
func (nm NodeMap) FindOrInsertNode(info NodeInfo, kept NodeSet) *Node {
	if kept != nil {
		if _, ok := kept[info]; !ok {
			return nil
		}
	}

	if n, ok := nm[info]; ok {
		return n
	}

	n := &Node{
		Info:        info,
		In:          make(EdgeMap),
		Out:         make(EdgeMap),
		LabelTags:   make(TagMap),
		NumericTags: make(map[string]TagMap),
	}
	nm[info] = n
	return n
}

// EdgeMap is used to represent the incoming/outgoing edges from a node.
type EdgeMap map[*Node]*Edge

// Edge contains any attributes to be represented about edges in a graph.
type Edge struct {
	Src, Dest *Node
	// The summary weight of the edge
	Weight int64
	// residual edges connect nodes that were connected through a
	// separate node, which has been removed from the report.
	Residual bool
	// An inline edge represents a call that was inlined into the caller.
	Inline bool
}

// Tag represent sample annotations
type Tag struct {
	Name  string
	Unit  string // Describe the value, "" for non-numeric tags
	Value int64
	Flat  int64
	Cum   int64
}

// TagMap is a collection of tags, classified by their name.
type TagMap map[string]*Tag

// SortTags sorts a slice of tags based on their weight.
func SortTags(t []*Tag, flat bool) []*Tag {
	ts := tags{t, flat}
	sort.Sort(ts)
	return ts.t
}

// New summarizes performance data from a profile into a graph.
func New(prof *profile.Profile, o *Options) *Graph {
	if o.CallTree {
		return newTree(prof, o)
	}
	g, _ := newGraph(prof, o)
	return g
}

// newGraph computes a graph from a profile. It returns the graph, and
// a map from the profile location indices to the corresponding graph
// nodes.
func newGraph(prof *profile.Profile, o *Options) (*Graph, map[uint64]Nodes) {
	nodes, locationMap := CreateNodes(prof, o.ObjNames, o.KeptNodes)
	for _, sample := range prof.Sample {
		weight := o.SampleValue(sample.Value)
		if weight == 0 {
			continue
		}
		seenNode := make(map[*Node]bool, len(sample.Location))
		seenEdge := make(map[nodePair]bool, len(sample.Location))
		var parent *Node
		// A residual edge goes over one or more nodes that were not kept.
		residual := false

		labels := joinLabels(sample)
		// Group the sample frames, based on a global map.
		for i := len(sample.Location) - 1; i >= 0; i-- {
			l := sample.Location[i]
			locNodes := locationMap[l.ID]
			for ni := len(locNodes) - 1; ni >= 0; ni-- {
				n := locNodes[ni]
				if n == nil {
					residual = true
					continue
				}
				// Add cum weight to all nodes in stack, avoiding double counting.
				if _, ok := seenNode[n]; !ok {
					seenNode[n] = true
					n.addSample(weight, labels, sample.NumLabel, o.FormatTag, false)
				}
				// Update edge weights for all edges in stack, avoiding double counting.
				if _, ok := seenEdge[nodePair{n, parent}]; !ok && parent != nil && n != parent {
					seenEdge[nodePair{n, parent}] = true
					parent.AddToEdge(n, weight, residual, ni != len(locNodes)-1)
				}
				parent = n
				residual = false
			}
		}
		if parent != nil && !residual {
			// Add flat weight to leaf node.
			parent.addSample(weight, labels, sample.NumLabel, o.FormatTag, true)
		}
	}

	return selectNodesForGraph(nodes, o.DropNegative), locationMap
}

func selectNodesForGraph(nodes Nodes, dropNegative bool) *Graph {
	// Collect nodes into a graph.
	gNodes := make(Nodes, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Cum == 0 && n.Flat == 0 {
			continue
		}
		if dropNegative && isNegative(n) {
			continue
		}
		gNodes = append(gNodes, n)
	}
	return &Graph{gNodes}
}

type nodePair struct {
	src, dest *Node
}

func newTree(prof *profile.Profile, o *Options) (g *Graph) {
	kept := o.KeptNodes
	keepBinary := o.ObjNames
	parentNodeMap := make(map[*Node]NodeMap, len(prof.Sample))
nextSample:
	for _, sample := range prof.Sample {
		weight := o.SampleValue(sample.Value)
		if weight == 0 {
			continue
		}
		var parent *Node
		labels := joinLabels(sample)
		// Group the sample frames, based on a per-node map.
		for i := len(sample.Location) - 1; i >= 0; i-- {
			l := sample.Location[i]
			lines := l.Line
			if len(lines) == 0 {
				lines = []profile.Line{{}} // Create empty line to include location info.
			}
			for lidx := len(lines) - 1; lidx >= 0; lidx-- {
				nodeMap := parentNodeMap[parent]
				if nodeMap == nil {
					nodeMap = make(NodeMap)
					parentNodeMap[parent] = nodeMap
				}
				n := nodeMap.findOrInsertLine(l, lines[lidx], keepBinary, kept)
				if n == nil {
					continue nextSample
				}
				n.addSample(weight, labels, sample.NumLabel, o.FormatTag, false)
				if parent != nil {
					parent.AddToEdge(n, weight, false, lidx != len(lines)-1)
				}
				parent = n
			}
		}
		if parent != nil {
			parent.addSample(weight, labels, sample.NumLabel, o.FormatTag, true)
		}
	}

	nodes := make(Nodes, len(prof.Location))
	for _, nm := range parentNodeMap {
		nodes = append(nodes, nm.nodes()...)
	}
	return selectNodesForGraph(nodes, o.DropNegative)
}

func joinLabels(s *profile.Sample) string {
	if len(s.Label) == 0 {
		return ""
	}

	var labels []string
	for key, vals := range s.Label {
		for _, v := range vals {
			labels = append(labels, key+":"+v)
		}
	}
	sort.Strings(labels)
	return strings.Join(labels, `\n`)
}

// isNegative returns true if the node is considered as "negative" for the
// purposes of drop_negative.
func isNegative(n *Node) bool {
	switch {
	case n.Flat < 0:
		return true
	case n.Flat == 0 && n.Cum < 0:
		return true
	default:
		return false
	}
}

// CreateNodes creates graph nodes for all locations in a profile.  It
// returns set of all nodes, plus a mapping of each location to the
// set of corresponding nodes (one per location.Line). If kept is
// non-nil, only nodes in that set are included; nodes that do not
// match are represented as a nil.
func CreateNodes(prof *profile.Profile, keepBinary bool, kept NodeSet) (Nodes, map[uint64]Nodes) {
	locations := make(map[uint64]Nodes, len(prof.Location))

	nm := make(NodeMap, len(prof.Location))
	for _, l := range prof.Location {
		lines := l.Line
		if len(lines) == 0 {
			lines = []profile.Line{{}} // Create empty line to include location info.
		}
		nodes := make(Nodes, len(lines))
		for ln := range lines {
			nodes[ln] = nm.findOrInsertLine(l, lines[ln], keepBinary, kept)
		}
		locations[l.ID] = nodes
	}
	return nm.nodes(), locations
}

func (nm NodeMap) nodes() Nodes {
	nodes := make(Nodes, 0, len(nm))
	for _, n := range nm {
		nodes = append(nodes, n)
	}
	return nodes
}

func (nm NodeMap) findOrInsertLine(l *profile.Location, li profile.Line, keepBinary bool, kept NodeSet) *Node {
	var objfile string
	if m := l.Mapping; m != nil && m.File != "" {
		objfile = filepath.Base(m.File)
	}

	if ni := nodeInfo(l, li, objfile, keepBinary); ni != nil {
		return nm.FindOrInsertNode(*ni, kept)
	}
	return nil
}

func nodeInfo(l *profile.Location, line profile.Line, objfile string, keepBinary bool) *NodeInfo {
	if line.Function == nil {
		return &NodeInfo{Address: l.Address, Objfile: objfile}
	}
	ni := &NodeInfo{
		Address:  l.Address,
		Lineno:   int(line.Line),
		Name:     line.Function.Name,
		OrigName: line.Function.SystemName,
	}
	if fname := line.Function.Filename; fname != "" {
		ni.File = filepath.Clean(fname)
	}
	if keepBinary {
		ni.Objfile = objfile
		ni.StartLine = int(line.Function.StartLine)
	}
	return ni
}

type tags struct {
	t    []*Tag
	flat bool
}

func (t tags) Len() int      { return len(t.t) }
func (t tags) Swap(i, j int) { t.t[i], t.t[j] = t.t[j], t.t[i] }
func (t tags) Less(i, j int) bool {
	if !t.flat {
		if t.t[i].Cum != t.t[j].Cum {
			return abs64(t.t[i].Cum) > abs64(t.t[j].Cum)
		}
	}
	if t.t[i].Flat != t.t[j].Flat {
		return abs64(t.t[i].Flat) > abs64(t.t[j].Flat)
	}
	return t.t[i].Name < t.t[j].Name
}

// Sum adds the flat and cum values of a set of nodes.
func (ns Nodes) Sum() (flat int64, cum int64) {
	for _, n := range ns {
		flat += n.Flat
		cum += n.Cum
	}
	return
}

func (n *Node) addSample(value int64, labels string, numLabel map[string][]int64, format func(int64, string) string, flat bool) {
	// Update sample value
	if flat {
		n.Flat += value
	} else {
		n.Cum += value
	}

	// Add string tags
	if labels != "" {
		t := n.LabelTags.findOrAddTag(labels, "", 0)
		if flat {
			t.Flat += value
		} else {
			t.Cum += value
		}
	}

	numericTags := n.NumericTags[labels]
	if numericTags == nil {
		numericTags = TagMap{}
		n.NumericTags[labels] = numericTags
	}
	// Add numeric tags
	if format == nil {
		format = defaultLabelFormat
	}
	for key, nvals := range numLabel {
		for _, v := range nvals {
			t := numericTags.findOrAddTag(format(v, key), key, v)
			if flat {
				t.Flat += value
			} else {
				t.Cum += value
			}
		}
	}
}

func defaultLabelFormat(v int64, key string) string {
	return strconv.FormatInt(v, 10)
}

func (m TagMap) findOrAddTag(label, unit string, value int64) *Tag {
	l := m[label]
	if l == nil {
		l = &Tag{
			Name:  label,
			Unit:  unit,
			Value: value,
		}
		m[label] = l
	}
	return l
}

// String returns a text representation of a graph, for debugging purposes.
func (g *Graph) String() string {
	var s []string

	nodeIndex := make(map[*Node]int, len(g.Nodes))

	for i, n := range g.Nodes {
		nodeIndex[n] = i + 1
	}

	for i, n := range g.Nodes {
		name := n.Info.PrintableName()
		var in, out []int

		for _, from := range n.In {
			in = append(in, nodeIndex[from.Src])
		}
		for _, to := range n.Out {
			out = append(out, nodeIndex[to.Dest])
		}
		s = append(s, fmt.Sprintf("%d: %s[flat=%d cum=%d] %x -> %v ", i+1, name, n.Flat, n.Cum, in, out))
	}
	return strings.Join(s, "\n")
}

// DiscardLowFrequencyNodes returns a set of the nodes at or over a
// specific cum value cutoff.
func (g *Graph) DiscardLowFrequencyNodes(nodeCutoff int64) NodeSet {
	return makeNodeSet(g.Nodes, nodeCutoff)
}

func makeNodeSet(nodes Nodes, nodeCutoff int64) NodeSet {
	kept := make(NodeSet, len(nodes))
	for _, n := range nodes {
		if abs64(n.Cum) < nodeCutoff {
			continue
		}
		kept[n.Info] = true
	}
	return kept
}

// TrimLowFrequencyTags removes tags that have less than
// the specified weight.
func (g *Graph) TrimLowFrequencyTags(tagCutoff int64) {
	// Remove nodes with value <= total*nodeFraction
	for _, n := range g.Nodes {
		n.LabelTags = trimLowFreqTags(n.LabelTags, tagCutoff)
		for s, nt := range n.NumericTags {
			n.NumericTags[s] = trimLowFreqTags(nt, tagCutoff)
		}
	}
}

func trimLowFreqTags(tags TagMap, minValue int64) TagMap {
	kept := TagMap{}
	for s, t := range tags {
		if abs64(t.Flat) >= minValue || abs64(t.Cum) >= minValue {
			kept[s] = t
		}
	}
	return kept
}

// TrimLowFrequencyEdges removes edges that have less than
// the specified weight. Returns the number of edges removed
func (g *Graph) TrimLowFrequencyEdges(edgeCutoff int64) int {
	var droppedEdges int
	for _, n := range g.Nodes {
		for src, e := range n.In {
			if abs64(e.Weight) < edgeCutoff {
				delete(n.In, src)
				delete(src.Out, n)
				droppedEdges++
			}
		}
	}
	return droppedEdges
}

// SortNodes sorts the nodes in a graph based on a specific heuristic.
func (g *Graph) SortNodes(cum bool, visualMode bool) {
	// Sort nodes based on requested mode
	switch {
	case visualMode:
		// Specialized sort to produce a more visually-interesting graph
		g.Nodes.Sort(EntropyOrder)
	case cum:
		g.Nodes.Sort(CumNameOrder)
	default:
		g.Nodes.Sort(FlatNameOrder)
	}
}

// SelectTopNodes returns a set of the top maxNodes nodes in a graph.
func (g *Graph) SelectTopNodes(maxNodes int, visualMode bool) NodeSet {
	if maxNodes > 0 {
		if visualMode {
			var count int
			// If generating a visual graph, count tags as nodes. Update
			// maxNodes to account for them.
			for i, n := range g.Nodes {
				if count += countTags(n) + 1; count >= maxNodes {
					maxNodes = i + 1
					break
				}
			}
		}
	}
	if maxNodes > len(g.Nodes) {
		maxNodes = len(g.Nodes)
	}
	return makeNodeSet(g.Nodes[:maxNodes], 0)
}

// countTags counts the tags with flat count. This underestimates the
// number of tags being displayed, but in practice is close enough.
func countTags(n *Node) int {
	count := 0
	for _, e := range n.LabelTags {
		if e.Flat != 0 {
			count++
		}
	}
	for _, t := range n.NumericTags {
		for _, e := range t {
			if e.Flat != 0 {
				count++
			}
		}
	}
	return count
}

// countEdges counts the number of edges below the specified cutoff.
func countEdges(el EdgeMap, cutoff int64) int {
	count := 0
	for _, e := range el {
		if e.Weight > cutoff {
			count++
		}
	}
	return count
}

// RemoveRedundantEdges removes residual edges if the destination can
// be reached through another path. This is done to simplify the graph
// while preserving connectivity.
func (g *Graph) RemoveRedundantEdges() {
	// Walk the nodes and outgoing edges in reverse order to prefer
	// removing edges with the lowest weight.
	for i := len(g.Nodes); i > 0; i-- {
		n := g.Nodes[i-1]
		in := n.In.Sort()
		for j := len(in); j > 0; j-- {
			e := in[j-1]
			if !e.Residual {
				// Do not remove edges heavier than a non-residual edge, to
				// avoid potential confusion.
				break
			}
			if isRedundant(e) {
				delete(e.Src.Out, e.Dest)
				delete(e.Dest.In, e.Src)
			}
		}
	}
}

// isRedundant determines if an edge can be removed without impacting
// connectivity of the whole graph. This is implemented by checking if the
// nodes have a common ancestor after removing the edge.
func isRedundant(e *Edge) bool {
	destPred := predecessors(e, e.Dest)
	if len(destPred) == 1 {
		return false
	}
	srcPred := predecessors(e, e.Src)

	for n := range srcPred {
		if destPred[n] && n != e.Dest {
			return true
		}
	}
	return false
}

// predecessors collects all the predecessors to node n, excluding edge e.
func predecessors(e *Edge, n *Node) map[*Node]bool {
	seen := map[*Node]bool{n: true}
	queue := Nodes{n}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, ie := range n.In {
			if e == ie || seen[ie.Src] {
				continue
			}
			seen[ie.Src] = true
			queue = append(queue, ie.Src)
		}
	}
	return seen
}

// nodeSorter is a mechanism used to allow a report to be sorted
// in different ways.
type nodeSorter struct {
	rs   Nodes
	less func(l, r *Node) bool
}

func (s nodeSorter) Len() int           { return len(s.rs) }
func (s nodeSorter) Swap(i, j int)      { s.rs[i], s.rs[j] = s.rs[j], s.rs[i] }
func (s nodeSorter) Less(i, j int) bool { return s.less(s.rs[i], s.rs[j]) }

// Sort reorders a slice of nodes based on the specified ordering
// criteria. The result is sorted in decreasing order for (absolute)
// numeric quantities, alphabetically for text, and increasing for
// addresses.
func (ns Nodes) Sort(o NodeOrder) error {
	var s nodeSorter

	switch o {
	case FlatNameOrder:
		s = nodeSorter{ns,
			func(l, r *Node) bool {
				if iv, jv := abs64(l.Flat), abs64(r.Flat); iv != jv {
					return iv > jv
				}
				if iv, jv := l.Info.PrintableName(), r.Info.PrintableName(); iv != jv {
					return iv < jv
				}
				if iv, jv := abs64(l.Cum), abs64(r.Cum); iv != jv {
					return iv > jv
				}
				return compareNodes(l, r)
			},
		}
	case FlatCumNameOrder:
		s = nodeSorter{ns,
			func(l, r *Node) bool {
				if iv, jv := abs64(l.Flat), abs64(r.Flat); iv != jv {
					return iv > jv
				}
				if iv, jv := abs64(l.Cum), abs64(r.Cum); iv != jv {
					return iv > jv
				}
				if iv, jv := l.Info.PrintableName(), r.Info.PrintableName(); iv != jv {
					return iv < jv
				}
				return compareNodes(l, r)
			},
		}
	case NameOrder:
		s = nodeSorter{ns,
			func(l, r *Node) bool {
				if iv, jv := l.Info.Name, r.Info.Name; iv != jv {
					return iv < jv
				}
				return compareNodes(l, r)
			},
		}
	case FileOrder:
		s = nodeSorter{ns,
			func(l, r *Node) bool {
				if iv, jv := l.Info.File, r.Info.File; iv != jv {
					return iv < jv
				}
				return compareNodes(l, r)
			},
		}
	case AddressOrder:
		s = nodeSorter{ns,
			func(l, r *Node) bool {
				if iv, jv := l.Info.Address, r.Info.Address; iv != jv {
					return iv < jv
				}
				return compareNodes(l, r)
			},
		}
	case CumNameOrder, EntropyOrder:
		// Hold scoring for score-based ordering
		var score map[*Node]int64
		scoreOrder := func(l, r *Node) bool {
			if iv, jv := abs64(score[l]), abs64(score[r]); iv != jv {
				return iv > jv
			}
			if iv, jv := l.Info.PrintableName(), r.Info.PrintableName(); iv != jv {
				return iv < jv
			}
			if iv, jv := abs64(l.Flat), abs64(r.Flat); iv != jv {
				return iv > jv
			}
			return compareNodes(l, r)
		}

		switch o {
		case CumNameOrder:
			score = make(map[*Node]int64, len(ns))
			for _, n := range ns {
				score[n] = n.Cum
			}
			s = nodeSorter{ns, scoreOrder}
		case EntropyOrder:
			score = make(map[*Node]int64, len(ns))
			for _, n := range ns {
				score[n] = entropyScore(n)
			}
			s = nodeSorter{ns, scoreOrder}
		}
	default:
		return fmt.Errorf("report: unrecognized sort ordering: %d", o)
	}
	sort.Sort(s)
	return nil
}

// compareNodes compares two nodes to provide a deterministic ordering
// between them. Two nodes cannot have the same Node.Info value.
func compareNodes(l, r *Node) bool {
	return fmt.Sprint(l.Info) < fmt.Sprint(r.Info)
}

// entropyScore computes a score for a node representing how important
// it is to include this node on a graph visualization. It is used to
// sort the nodes and select which ones to display if we have more
// nodes than desired in the graph. This number is computed by looking
// at the flat and cum weights of the node and the incoming/outgoing
// edges. The fundamental idea is to penalize nodes that have a simple
// fallthrough from their incoming to the outgoing edge.
func entropyScore(n *Node) int64 {
	score := float64(0)

	if len(n.In) == 0 {
		score++ // Favor entry nodes
	} else {
		score += edgeEntropyScore(n, n.In, 0)
	}

	if len(n.Out) == 0 {
		score++ // Favor leaf nodes
	} else {
		score += edgeEntropyScore(n, n.Out, n.Flat)
	}

	return int64(score*float64(n.Cum)) + n.Flat
}

// edgeEntropyScore computes the entropy value for a set of edges
// coming in or out of a node. Entropy (as defined in information
// theory) refers to the amount of information encoded by the set of
// edges. A set of edges that have a more interesting distribution of
// samples gets a higher score.
func edgeEntropyScore(n *Node, edges EdgeMap, self int64) float64 {
	score := float64(0)
	total := self
	for _, e := range edges {
		if e.Weight > 0 {
			total += abs64(e.Weight)
		}
	}
	if total != 0 {
		for _, e := range edges {
			frac := float64(abs64(e.Weight)) / float64(total)
			score += -frac * math.Log2(frac)
		}
		if self > 0 {
			frac := float64(abs64(self)) / float64(total)
			score += -frac * math.Log2(frac)
		}
	}
	return score
}

// NodeOrder sets the ordering for a Sort operation
type NodeOrder int

// Sorting options for node sort.
const (
	FlatNameOrder NodeOrder = iota
	FlatCumNameOrder
	CumNameOrder
	NameOrder
	FileOrder
	AddressOrder
	EntropyOrder
)

// Sort returns a slice of the edges in the map, in a consistent
// order. The sort order is first based on the edge weight
// (higher-to-lower) and then by the node names to avoid flakiness.
func (e EdgeMap) Sort() []*Edge {
	el := make(edgeList, 0, len(e))
	for _, w := range e {
		el = append(el, w)
	}

	sort.Sort(el)
	return el
}

// Sum returns the total weight for a set of nodes.
func (e EdgeMap) Sum() int64 {
	var ret int64
	for _, edge := range e {
		ret += edge.Weight
	}
	return ret
}

type edgeList []*Edge

func (el edgeList) Len() int {
	return len(el)
}

func (el edgeList) Less(i, j int) bool {
	if el[i].Weight != el[j].Weight {
		return abs64(el[i].Weight) > abs64(el[j].Weight)
	}

	from1 := el[i].Src.Info.PrintableName()
	from2 := el[j].Src.Info.PrintableName()
	if from1 != from2 {
		return from1 < from2
	}

	to1 := el[i].Dest.Info.PrintableName()
	to2 := el[j].Dest.Info.PrintableName()

	return to1 < to2
}

func (el edgeList) Swap(i, j int) {
	el[i], el[j] = el[j], el[i]
}

func abs64(i int64) int64 {
	if i < 0 {
		return -i
	}
	return i
}
