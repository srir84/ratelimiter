"""Independent S5 reference model."""


class RecordError(ValueError):
    """Malformed input, with its record and field identified."""

    def __init__(self, record: str, field: str, reason: str) -> None:
        super().__init__(f"{record}: field {field}: {reason}")
