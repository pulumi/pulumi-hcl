invokeResult = invoke("primitive:index:invoke", {
  boolean     = plainString
  float       = plainInteger
  integer     = plainNumericString
  string      = plainBool
  numberArray = [plainInteger, plainNumericString, plainNumber]
  booleanMap = {
    "fromBool"   = plainBool
    "fromString" = plainString
  }
})

resource "plainValues" "primitive:index:Resource" {
  boolean     = plainString
  float       = plainInteger
  integer     = plainNumericString
  string      = plainNumber
  numberArray = [plainInteger, plainNumericString, plainNumber]
  booleanMap = {
    "fromBool"   = plainBool
    "fromString" = plainString
  }
}

resource "secretValues" "primitive:index:Resource" {
  boolean     = secretString
  float       = secretInteger
  integer     = secretNumericString
  string      = secretNumber
  numberArray = [plainInteger, plainNumericString, plainNumber]
  booleanMap = {
    "fromBool"   = plainBool
    "fromString" = plainString
  }
}

resource "invokeValues" "primitive:index:Resource" {
  boolean     = invokeResult.boolean
  float       = invokeResult.float
  integer     = invokeResult.integer
  string      = invokeResult.string
  numberArray = invokeResult.numberArray
  booleanMap  = invokeResult.booleanMap
}

config "plainBool" "bool" {
}

config "plainNumber" "number" {
}

config "plainInteger" "number" {
}

config "plainString" "string" {
}

config "plainNumericString" "string" {
}

config "secretNumber" "number" {
}

config "secretInteger" "number" {
}

config "secretString" "string" {
}

config "secretNumericString" "string" {
}

