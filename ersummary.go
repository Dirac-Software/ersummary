package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/path"
	"gonum.org/v1/gonum/graph/simple"
)

type Table struct {
	Name    string
	Schema  string
	Columns []Column
}

type Column struct {
	Name     string
	DataType string
	IsPK     bool
	IsFK     bool
}

type ForeignKey struct {
	FromTable      string
	FromColumn     string
	ToTable        string
	ToColumn       string
	ConstraintName string
}

type Cardinality struct {
	Min string
	Max string
}

type Relationship struct {
	From            Table
	To              Table
	FromCardinality Cardinality
	ToCardinality   Cardinality
	Path            []string // Tables in the path
}

type ColumnInfo struct {
	IsNullable          bool
	HasUniqueConstraint bool
}

func main() {
	var connStr string
	var schema string
	var tablesStr string
	var showColumns bool

	flag.StringVar(&connStr, "conn", "", "PostgreSQL connection string")
	flag.StringVar(&schema, "schema", "public", "Database schema")
	flag.StringVar(&tablesStr, "tables", "", "Comma-separated list of tables")
	flag.BoolVar(&showColumns, "show-columns", false, "Show table columns in the diagram")
	flag.Parse()

	if connStr == "" || tablesStr == "" {
		log.Fatal("Connection string and tables list are required")
	}

	tables := strings.Split(tablesStr, ",")
	for i := range tables {
		tables[i] = strings.TrimSpace(tables[i])
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Error connecting to database:", err)
	}
	defer db.Close()

	err = db.Ping()
	if err != nil {
		log.Fatal("Error pinging database:", err)
	}

	// Get ALL foreign keys in the schema to build complete graph
	allForeignKeys, err := getAllForeignKeys(db, schema)
	if err != nil {
		log.Fatal("Error fetching all foreign keys:", err)
	}

	// Filter foreign keys for selected tables (for column display)
	selectedForeignKeys := filterForeignKeys(allForeignKeys, tables)

	relationships := calculateCardinalities(db, schema, tables, allForeignKeys)

	var tableDetails []Table
	if showColumns {
		tableDetails, err = getTableColumns(db, schema, tables, selectedForeignKeys)
		if err != nil {
			log.Fatal("Error fetching table columns:", err)
		}
	} else {
		for _, tableName := range tables {
			tableDetails = append(tableDetails, Table{Name: tableName, Schema: schema})
		}
	}

	// Build command line for comment
	cmdLine := append([]string{os.Args[0]}, os.Args[1:]...)
	mermaidDiagram := generateMermaidDiagram(tableDetails, relationships, schema, strings.Join(cmdLine, " "))
	fmt.Println(mermaidDiagram)
}

func getAllForeignKeys(db *sql.DB, schema string) ([]ForeignKey, error) {
	query := `
		SELECT 
			tc.table_name AS from_table,
			kcu.column_name AS from_column,
			ccu.table_name AS to_table,
			ccu.column_name AS to_column,
			tc.constraint_name
		FROM 
			information_schema.table_constraints AS tc 
			JOIN information_schema.key_column_usage AS kcu
				ON tc.constraint_name = kcu.constraint_name
				AND tc.table_schema = kcu.table_schema
			JOIN information_schema.constraint_column_usage AS ccu
				ON ccu.constraint_name = tc.constraint_name
				AND ccu.table_schema = tc.table_schema
		WHERE 
			tc.constraint_type = 'FOREIGN KEY' 
			AND tc.table_schema = $1
	`

	log.Printf("Fetching all foreign keys from schema '%s'...", schema)
	start := time.Now()
	rows, err := db.Query(query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var foreignKeys []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		err := rows.Scan(&fk.FromTable, &fk.FromColumn, &fk.ToTable, &fk.ToColumn, &fk.ConstraintName)
		if err != nil {
			return nil, err
		}
		foreignKeys = append(foreignKeys, fk)
	}

	log.Printf("Found %d foreign keys in schema '%s' (took %v)", len(foreignKeys), schema, time.Since(start))
	return foreignKeys, rows.Err()
}

func filterForeignKeys(allForeignKeys []ForeignKey, tables []string) []ForeignKey {
	tableSet := make(map[string]bool)
	for _, table := range tables {
		tableSet[table] = true
	}

	var filtered []ForeignKey
	for _, fk := range allForeignKeys {
		if tableSet[fk.FromTable] && tableSet[fk.ToTable] {
			filtered = append(filtered, fk)
		}
	}

	log.Printf("Filtered %d foreign keys for selected tables (from %d total)", len(filtered), len(allForeignKeys))
	return filtered
}

type TableNode struct {
	id   int64
	name string
}

func (n TableNode) ID() int64 {
	return n.id
}

func calculateCardinalities(db *sql.DB, schema string, selectedTables []string, allForeignKeys []ForeignKey) []Relationship {
	if len(allForeignKeys) == 0 {
		return []Relationship{}
	}

	// Build directed graph using gonum
	g := simple.NewDirectedGraph()
	tableToNode := make(map[string]graph.Node)
	nodeToTable := make(map[int64]string)
	nodeID := int64(0)

	// Add all tables as nodes
	for _, fk := range allForeignKeys {
		if _, exists := tableToNode[fk.FromTable]; !exists {
			node := TableNode{id: nodeID, name: fk.FromTable}
			g.AddNode(node)
			tableToNode[fk.FromTable] = node
			nodeToTable[nodeID] = fk.FromTable
			nodeID++
		}
		if _, exists := tableToNode[fk.ToTable]; !exists {
			node := TableNode{id: nodeID, name: fk.ToTable}
			g.AddNode(node)
			tableToNode[fk.ToTable] = node
			nodeToTable[nodeID] = fk.ToTable
			nodeID++
		}
	}

	// Add edges for foreign keys (inverted direction)
	// FK goes from child to parent, but we want edges from parent to child
	// to find common descendants
	for _, fk := range allForeignKeys {
		if fk.FromTable == fk.ToTable {
			continue // Skip self-references
		}
		fromNode := tableToNode[fk.FromTable]
		toNode := tableToNode[fk.ToTable]
		// Invert the edge direction: parent -> child
		g.SetEdge(g.NewEdge(toNode, fromNode))
	}

	// Use gonum's Floyd-Warshall
	allPaths, ok := path.FloydWarshall(g)
	if !ok {
		log.Printf("Floyd-Warshall found negative cycles")
		return []Relationship{}
	}

	// Get column info for all FK columns in one query
	columnInfo, err := getColumnInfo(db, schema, allForeignKeys)
	if err != nil {
		log.Printf("Error fetching column info: %v", err)
		return []Relationship{}
	}

	// Create FK lookup map
	fkMap := make(map[string]ForeignKey)
	for _, fk := range allForeignKeys {
		fkMap[fk.FromTable+"->"+fk.ToTable] = fk
	}

	// Create map of selected tables for quick lookup
	selectedMap := make(map[string]bool)
	for _, t := range selectedTables {
		selectedMap[t] = true
	}

	var relationships []Relationship

	// Find relationships between all pairs of selected tables
	for i, tableA := range selectedTables {
		for j, tableB := range selectedTables {
			if i >= j { // Skip self and already processed pairs
				continue
			}

			nodeA, okA := tableToNode[tableA]
			nodeB, okB := tableToNode[tableB]
			if !okA || !okB {
				continue
			}

			// Find common descendant (highest table with FKs to both A and B)
			lca, lcaNode := findLCAUsingGonum(tableA, tableB, g, tableToNode, nodeToTable, selectedMap, allPaths)
			if lca != "" {
				// Get paths from A to C and B to C (inverted graph)
				pathAtoC, _, _ := allPaths.Between(nodeA.ID(), lcaNode.ID())
				pathBtoC, _, _ := allPaths.Between(nodeB.ID(), lcaNode.ID())

				if len(pathAtoC) > 0 && len(pathBtoC) > 0 {
					// Convert to string paths and reverse them to get C->A and C->B
					strPathAtoC := nodesToTables(pathAtoC, nodeToTable)
					strPathBtoC := nodesToTables(pathBtoC, nodeToTable)

					// Reverse paths to get the actual FK direction
					strPathCtoA := make([]string, len(strPathAtoC))
					strPathCtoB := make([]string, len(strPathBtoC))
					for i := range strPathAtoC {
						strPathCtoA[i] = strPathAtoC[len(strPathAtoC)-1-i]
					}
					for i := range strPathBtoC {
						strPathCtoB[i] = strPathBtoC[len(strPathBtoC)-1-i]
					}

					// Calculate combined cardinality
					relationship := calculateLCACardinality(lca, tableA, tableB, strPathCtoA, strPathCtoB, fkMap, columnInfo, schema)
					if relationship != nil {
						relationships = append(relationships, *relationship)
					}
				}
			} else {
				// Direct path between A and B (considering inverted graph)
				rel := findDirectPath(nodeA, nodeB, tableA, tableB, allPaths, nodeToTable, selectedMap, fkMap, columnInfo, schema)
				if rel != nil {
					relationships = append(relationships, *rel)
				}
			}
		}
	}

	return relationships
}

func nodesToTables(nodes []graph.Node, nodeToTable map[int64]string) []string {
	tables := make([]string, len(nodes))
	for i, node := range nodes {
		tables[i] = nodeToTable[node.ID()]
	}
	return tables
}

func isValidPath(path []string, selectedMap map[string]bool) bool {
	// Check intermediate nodes (not start or end)
	for i := 1; i < len(path)-1; i++ {
		if selectedMap[path[i]] {
			return false
		}
	}
	return true
}

func findDirectPath(nodeA, nodeB graph.Node, tableA, tableB string, allPaths path.AllShortest, nodeToTable map[int64]string, selectedMap map[string]bool, fkMap map[string]ForeignKey, columnInfo map[string]ColumnInfo, schema string) *Relationship {
	// Try path from B to A (means A has FK to B in inverted graph)
	if rel := tryDirectPath(nodeB, nodeA, allPaths, nodeToTable, selectedMap, fkMap, columnInfo, schema); rel != nil {
		return rel
	}

	// Try path from A to B (means B has FK to A in inverted graph)
	return tryDirectPath(nodeA, nodeB, allPaths, nodeToTable, selectedMap, fkMap, columnInfo, schema)
}

func tryDirectPath(fromNode, toNode graph.Node, allPaths path.AllShortest, nodeToTable map[int64]string, selectedMap map[string]bool, fkMap map[string]ForeignKey, columnInfo map[string]ColumnInfo, schema string) *Relationship {
	path, _, _ := allPaths.Between(fromNode.ID(), toNode.ID())
	if len(path) == 0 {
		return nil
	}

	strPath := nodesToTables(path, nodeToTable)
	if !isValidPath(strPath, selectedMap) {
		return nil
	}

	// Reverse to get actual FK direction (graph is inverted)
	reversedPath := make([]string, len(strPath))
	for i := range strPath {
		reversedPath[i] = strPath[len(strPath)-1-i]
	}

	relationship := calculatePathCardinality(reversedPath, fkMap, columnInfo, schema)
	if relationship != nil {
		relationship.Path = reversedPath
	}
	return relationship
}

func findLCAUsingGonum(tableA, tableB string, g graph.Directed, tableToNode map[string]graph.Node, nodeToTable map[int64]string, selectedMap map[string]bool, allPaths path.AllShortest) (string, graph.Node) {
	nodeA := tableToNode[tableA]
	nodeB := tableToNode[tableB]

	// Find all nodes that can reach both A and B (common descendants)
	// Since graph is inverted, paths from C to A mean C has FK path to A
	var candidates []graph.Node
	nodes := g.Nodes()
	for nodes.Next() {
		node := nodes.Node()

		// Check if A and B can reach this node (meaning this node has FKs to A and B)
		pathFromA, _, _ := allPaths.Between(nodeA.ID(), node.ID())
		pathFromB, _, _ := allPaths.Between(nodeB.ID(), node.ID())

		if len(pathFromA) > 0 && len(pathFromB) > 0 {
			// Check if paths don't go through other selected tables
			strPathFromA := nodesToTables(pathFromA, nodeToTable)
			strPathFromB := nodesToTables(pathFromB, nodeToTable)

			if isValidPath(strPathFromA, selectedMap) && isValidPath(strPathFromB, selectedMap) {
				candidates = append(candidates, node)
			}
		}
	}

	// Among candidates, find the one with minimum total distance
	if len(candidates) == 0 {
		return "", nil
	}

	var bestLCA graph.Node
	minDist := float64(1000000)

	for _, candidate := range candidates {
		distToA := allPaths.Weight(candidate.ID(), nodeA.ID())
		distToB := allPaths.Weight(candidate.ID(), nodeB.ID())
		totalDist := distToA + distToB

		if totalDist < minDist {
			minDist = totalDist
			bestLCA = candidate
		}
	}

	if bestLCA == nil {
		return "", nil
	}

	return nodeToTable[bestLCA.ID()], bestLCA
}

func calculateLCACardinality(lca, tableA, tableB string, pathCtoA, pathCtoB []string, fkMap map[string]ForeignKey, columnInfo map[string]ColumnInfo, schema string) *Relationship {
	// Calculate cardinalities along both paths
	cardCtoA := calculatePathCardinality(pathCtoA, fkMap, columnInfo, schema)
	cardCtoB := calculatePathCardinality(pathCtoB, fkMap, columnInfo, schema)

	if cardCtoA == nil || cardCtoB == nil {
		return nil
	}

	// Combine cardinalities through the LCA
	// The relationship from A to B through C depends on both paths
	// If C->A is 1:* and C->B is 1:*, then A->B is *:*
	fromMin := "0"
	fromMax := "*"
	toMin := "0"
	toMax := "*"

	// If both paths have required relationships (min = 1), then the combined is also required
	if cardCtoA.ToCardinality.Min == "1" && cardCtoB.ToCardinality.Min == "1" {
		fromMin = "1"
		toMin = "1"
	}

	// Build the complete path
	fullPath := make([]string, 0)
	// Reverse path from A to C
	for i := len(pathCtoA) - 1; i >= 0; i-- {
		fullPath = append(fullPath, pathCtoA[i])
	}
	// Add path from C to B (excluding C which is already in the path)
	if len(pathCtoB) > 1 {
		fullPath = append(fullPath, pathCtoB[1:]...)
	}

	return &Relationship{
		From:            Table{Name: tableA, Schema: schema},
		To:              Table{Name: tableB, Schema: schema},
		FromCardinality: Cardinality{Min: fromMin, Max: fromMax},
		ToCardinality:   Cardinality{Min: toMin, Max: toMax},
		Path:            fullPath,
	}
}

func getColumnInfo(db *sql.DB, schema string, foreignKeys []ForeignKey) (map[string]ColumnInfo, error) {
	if len(foreignKeys) == 0 {
		return make(map[string]ColumnInfo), nil
	}

	// Build column info query
	var columnSpecs []string
	for _, fk := range foreignKeys {
		columnSpecs = append(columnSpecs, fmt.Sprintf("('%s', '%s', '%s')", fk.FromTable, fk.FromColumn, fk.FromTable+"."+fk.FromColumn))
	}

	query := fmt.Sprintf(`
		WITH fk_columns AS (
			SELECT * FROM (VALUES %s) AS t(table_name, column_name, table_column)
		),
		column_info AS (
			SELECT 
				fk.table_column,
				c.is_nullable = 'YES' as is_nullable,
				EXISTS (
					SELECT 1
					FROM information_schema.table_constraints tc
					JOIN information_schema.key_column_usage kcu 
						ON tc.constraint_name = kcu.constraint_name
					WHERE tc.table_schema = $1 
						AND tc.table_name = fk.table_name 
						AND kcu.column_name = fk.column_name
						AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
						-- Check that this is the only column in the constraint
						AND NOT EXISTS (
							SELECT 1 
							FROM information_schema.key_column_usage kcu2
							WHERE kcu2.constraint_name = tc.constraint_name
								AND kcu2.table_schema = tc.table_schema
								AND kcu2.column_name != fk.column_name
						)
				) as has_unique_constraint
			FROM fk_columns fk
			JOIN information_schema.columns c
				ON c.table_schema = $1
				AND c.table_name = fk.table_name
				AND c.column_name = fk.column_name
		)
		SELECT table_column, is_nullable, has_unique_constraint
		FROM column_info
	`, strings.Join(columnSpecs, ", "))

	log.Printf("Fetching column info for %d foreign key columns...", len(foreignKeys))
	start := time.Now()
	rows, err := db.Query(query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columnInfo := make(map[string]ColumnInfo)

	for rows.Next() {
		var tableColumn string
		var isNullable, hasUnique bool
		if err := rows.Scan(&tableColumn, &isNullable, &hasUnique); err != nil {
			return nil, err
		}
		columnInfo[tableColumn] = ColumnInfo{
			IsNullable:          isNullable,
			HasUniqueConstraint: hasUnique,
		}
	}

	log.Printf("Retrieved column info for %d columns (took %v)", len(columnInfo), time.Since(start))
	return columnInfo, nil
}

func calculatePathCardinality(pathTables []string, fkMap map[string]ForeignKey, columnInfo map[string]ColumnInfo, schema string) *Relationship {
	if len(pathTables) < 2 {
		return nil
	}

	// For now, we'll calculate cardinality for direct relationships
	// This can be extended to handle multi-hop paths
	if len(pathTables) == 2 {
		fromTable := pathTables[0]
		toTable := pathTables[1]

		// Check both directions for FK
		if fk, exists := fkMap[fromTable+"->"+toTable]; exists {
			return calculateDirectCardinality(fromTable, toTable, fk, columnInfo, schema)
		} else if fk, exists := fkMap[toTable+"->"+fromTable]; exists {
			// Swap the tables to get the correct direction
			rel := calculateDirectCardinality(toTable, fromTable, fk, columnInfo, schema)
			if rel != nil {
				// Swap the relationship direction
				return &Relationship{
					From:            rel.To,
					To:              rel.From,
					FromCardinality: rel.ToCardinality,
					ToCardinality:   rel.FromCardinality,
				}
			}
			return nil
		}
	}

	// For multi-hop paths, aggregate cardinalities
	// This is a simplified version - you might want to implement more sophisticated logic
	return &Relationship{
		From:            Table{Name: pathTables[0], Schema: schema},
		To:              Table{Name: pathTables[len(pathTables)-1], Schema: schema},
		FromCardinality: Cardinality{Min: "0", Max: "*"},
		ToCardinality:   Cardinality{Min: "0", Max: "*"},
	}
}

func calculateDirectCardinality(fromTable, toTable string, fk ForeignKey, columnInfo map[string]ColumnInfo, schema string) *Relationship {
	tableColumn := fk.FromTable + "." + fk.FromColumn
	info, found := columnInfo[tableColumn]

	min := "0"
	max := "*"

	if found {
		if !info.IsNullable {
			min = "1"
		}
		if info.HasUniqueConstraint {
			max = "1"
		}
	}
	return &Relationship{
		From:            Table{Name: fromTable, Schema: schema},
		To:              Table{Name: toTable, Schema: schema},
		FromCardinality: Cardinality{Min: min, Max: max},
		ToCardinality:   Cardinality{Min: "1", Max: "1"},
	}
}

func getTableColumns(db *sql.DB, schema string, tables []string, foreignKeys []ForeignKey) ([]Table, error) {
	tableList := "'" + strings.Join(tables, "','") + "'"

	// Create FK lookup map
	fkLookup := make(map[string]bool)
	for _, fk := range foreignKeys {
		fkLookup[fk.FromTable+"."+fk.FromColumn] = true
	}

	query := fmt.Sprintf(`
		SELECT 
			c.table_name,
			c.column_name,
			c.data_type,
			COALESCE(tc.constraint_type = 'PRIMARY KEY', false) as is_pk
		FROM 
			information_schema.columns c
		LEFT JOIN information_schema.key_column_usage kcu 
			ON c.table_schema = kcu.table_schema 
			AND c.table_name = kcu.table_name 
			AND c.column_name = kcu.column_name
		LEFT JOIN information_schema.table_constraints tc 
			ON kcu.constraint_name = tc.constraint_name 
			AND kcu.table_schema = tc.table_schema
			AND tc.constraint_type = 'PRIMARY KEY'
		WHERE 
			c.table_schema = $1
			AND c.table_name IN (%s)
		ORDER BY 
			c.table_name, 
			c.ordinal_position
	`, tableList)

	log.Printf("Fetching table columns for: %s", strings.Join(tables, ", "))
	start := time.Now()
	rows, err := db.Query(query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tableMap := make(map[string]*Table)
	for _, tableName := range tables {
		tableMap[tableName] = &Table{Name: tableName, Schema: schema, Columns: []Column{}}
	}

	for rows.Next() {
		var tableName, columnName, dataType string
		var isPK bool
		err := rows.Scan(&tableName, &columnName, &dataType, &isPK)
		if err != nil {
			return nil, err
		}

		if table, ok := tableMap[tableName]; ok {
			isFK := fkLookup[tableName+"."+columnName]
			table.Columns = append(table.Columns, Column{
				Name:     columnName,
				DataType: dataType,
				IsPK:     isPK,
				IsFK:     isFK,
			})
		}
	}

	var result []Table
	for _, tableName := range tables {
		if table, ok := tableMap[tableName]; ok {
			result = append(result, *table)
		}
	}

	log.Printf("Retrieved column details for %d tables (took %v)", len(result), time.Since(start))
	return result, rows.Err()
}

func generateMermaidDiagram(tables []Table, relationships []Relationship, schema string, commandLine string) string {
	var sb strings.Builder
	
	// Add comments at the top
	sb.WriteString("%%{init: {'theme':'neutral'}}%%\n")
	sb.WriteString("%% Generated by https://github.com/Dirac-Software/ersummary\n")
	sb.WriteString(fmt.Sprintf("%%%% Command: %s\n", commandLine))
	sb.WriteString("\nerDiagram\n")

	for _, table := range tables {
		sb.WriteString(fmt.Sprintf("    %s {\n", table.Name))
		if len(table.Columns) > 0 {
			for _, col := range table.Columns {
				keyIndicator := ""
				if col.IsPK && col.IsFK {
					keyIndicator = "PK,FK"
				} else if col.IsPK {
					keyIndicator = "PK"
				} else if col.IsFK {
					keyIndicator = "FK"
				}
				sb.WriteString(fmt.Sprintf("        %s %s %s\n", dataTypeToMermaid(col.DataType), col.Name, keyIndicator))
			}
		}
		sb.WriteString("    }\n")
	}

	for _, rel := range relationships {
		relType := getMermaidRelationType(rel.FromCardinality, rel.ToCardinality)
		label := ""
		if len(rel.Path) > 2 {
			label = fmt.Sprintf("via %s", strings.Join(rel.Path[1:len(rel.Path)-1], ", "))
		}
		sb.WriteString(fmt.Sprintf("    %s %s %s : \"%s\"\n",
			rel.From.Name,
			relType,
			rel.To.Name,
			label))
	}

	return sb.String()
}

func dataTypeToMermaid(pgType string) string {
	switch {
	case strings.Contains(pgType, "int"):
		return "int"
	case strings.Contains(pgType, "char"), strings.Contains(pgType, "text"):
		return "string"
	case strings.Contains(pgType, "timestamp"), strings.Contains(pgType, "date"), strings.Contains(pgType, "time"):
		return "datetime"
	case strings.Contains(pgType, "bool"):
		return "boolean"
	case strings.Contains(pgType, "numeric"), strings.Contains(pgType, "decimal"), strings.Contains(pgType, "real"), strings.Contains(pgType, "double"):
		return "float"
	default:
		return pgType
	}
}

func getMermaidRelationType(fromCard, toCard Cardinality) string {
	return getCardinalitySymbol(fromCard) + "--" + reverseString(getCardinalitySymbol(toCard))
}

func getCardinalitySymbol(card Cardinality) string {
	minMax := card.Min + card.Max
	switch minMax {
	case "01":
		return "|o"
	case "11":
		return "||"
	case "0*":
		return "}o"
	case "1*":
		return "}|"
	default:
		log.Printf("Unexpected cardinality: min=%s, max=%s", card.Min, card.Max)
		return "||"
	}
}

func reverseString(s string) string {
	runes := []rune(s)
	// First reverse the string
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	// Then flip the curly braces
	for i := range runes {
		switch runes[i] {
		case '{':
			runes[i] = '}'
		case '}':
			runes[i] = '{'
		}
	}
	return string(runes)
}
