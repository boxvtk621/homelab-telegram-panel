- source_spec: https://youtrack.h1-cloud.ru/issue/HL-294 (Task Revision 1); parent https://youtrack.h1-cloud.ru/issue/HL-279 (Story Revision 2)
  summary: Verify HL-294 R11-R12 PostgreSQL replica latency, restart/reconnect backfill, and hash canonicalization in a separate bounded package.
  evidence: R10 enrollment and history replication are independently testable deliveries; combining them would obscure exact AC and evidence boundaries.

- source_spec: https://youtrack.h1-cloud.ru/issue/HL-296 (Task Revision 1); parent Story revision must be read again at package start
  summary: Reassess HL-296 R14 Task closure against retained-data producer and deployed/runtime gates after the replica package.
  evidence: R14 search acceptance is independently shippable and depends on retained/retired data producers not owned by the R10 enrollment package.
