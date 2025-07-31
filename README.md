# ersummary
A tool to generate summary ERD from a subset of tables in a PostgreSQL database

When first approaching a complex data model, it helps to focus on a subset of
key tables. This tool generates an Entity-Relation Diagram (ERD) in
[MermaidJs][1] format, much as [Mermerd][2] does, with the following
differences:
* it accurately describes the relationship between tables even when they are
  mediated by junction tables that are not in the set of tables under
  consideration
* it is much faster, because it queries the data dictionary in a single pass


1: https://mermaid.js.org/syntax/entityRelationshipDiagram.html
2: https://github.com/KarnerTh/mermerd
