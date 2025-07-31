# ersummary
A tool to generate summary ERD from a subset of tables in a PostgreSQL database

# Description

When first approaching a complex data model, it helps to focus on a subset of
key tables. This tool generates an Entity-Relation Diagram (ERD) in
[MermaidJs](https://mermaid.js.org/syntax/entityRelationshipDiagram.html) format, much as [Mermerd](https://github.com/KarnerTh/mermerd) does, with the following
differences:
* it accurately describes the relationship between tables even when they are
  mediated by junction tables that are not in the set of tables under
  consideration
* it is much faster, because it queries the data dictionary in a single pass

# Installation

To build, you need Go installed, then simply run `go build` in this directory.

