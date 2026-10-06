---
inclusion: always
---

# Regla de CHANGELOG

TODO cambio que se realice en el proyecto DEBE registrarse en el archivo `CHANGELOG.md`
ubicado en la raíz del proyecto.

## Reglas obligatorias

1. Por CADA cambio (creación, edición o eliminación de archivos, correcciones,
   nuevas funcionalidades, refactors, etc.) se debe agregar una entrada en `CHANGELOG.md`.
2. El contenido de las entradas se escribe SIEMPRE en **español**.
3. Cada entrada debe incluir la marca de tiempo con:
   - Fecha en formato `yyyy:mm:dd`
   - Hora en formato `hora:minuto` (24 horas)
4. Las entradas nuevas se agregan al inicio del archivo (las más recientes primero).
5. La descripción debe ser breve y clara, indicando qué se cambió y por qué.
6. Cada entrada debe incluir un **resumen de los cambios** realizados.
7. Cada entrada debe incluir la **lista de archivos modificados** (creados, editados o eliminados).

## Formato de cada entrada

```
## yyyy:mm:dd hora:minuto

**Resumen:** Breve resumen de los cambios en español.

**Cambios:**
- Descripción del cambio en español.

**Archivos modificados:**
- `ruta/al/archivo1.ext`
- `ruta/al/archivo2.ext`
```

## Ejemplo

```
## 2026:09:08 14:30

**Resumen:** Se mejoró el inicio de sesión y se corrigió la tabla de usuarios.

**Cambios:**
- Se agregó la validación del formulario de inicio de sesión.
- Se corrigió el error de paginación en la tabla de usuarios.

**Archivos modificados:**
- `src/features/auth/LoginForm.tsx`
- `src/components/ui/Table.tsx`
```
