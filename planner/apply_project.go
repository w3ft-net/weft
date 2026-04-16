package planner

// This file previously contained ApplyProjection and the three projection
// rewrite functions. Projection is now baked directly into the SQL by the
// planner — see extractRawExpr in project.go and sql.go's setSelect/build.
